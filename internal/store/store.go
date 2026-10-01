// Package store: acceso a Postgres para el plano de control.
// Modelo v2: tickets + plan de ejecución DINÁMICO por ticket (las fases las genera el agente).
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func New(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 5
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("conectar postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                          { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error  { return s.pool.Ping(ctx) }

func (s *Store) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := s.Ping(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres no respondió en %s", timeout)
		}
		time.Sleep(time.Second)
	}
}

// ─────────────── entidades ───────────────

type Stage struct {
	Number      int     `json:"number"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Description *string `json:"description,omitempty"`
	DependsOn   *int    `json:"depends_on,omitempty"`
}

// PlanStage: una fase del plan que envía el agente en registrar_plan.
type PlanStage struct {
	Name        string
	Description string
	DependsOn   *int
}

type Ticket struct {
	ID     string
	Title  string
	Body   string
	Status string
}

// ─────────────── project / agent / session ───────────────

func (s *Store) EnsureProject(ctx context.Context, name string) (string, error) {
	if name == "" {
		name = "onixguard"
	}
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM projects WHERE name=$1 LIMIT 1`, name).Scan(&id); err == nil {
		return id, nil
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO projects (name) VALUES ($1) RETURNING id`, name).Scan(&id)
	return id, err
}

func (s *Store) EnsureAgent(ctx context.Context, projectID, role string) (string, error) {
	if role == "" {
		role = "project_lead"
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO agents (project_id, role) VALUES ($1,$2)
		 ON CONFLICT (project_id, role) DO UPDATE SET role=EXCLUDED.role RETURNING id`,
		projectID, role).Scan(&id)
	return id, err
}

func (s *Store) EnsureSession(ctx context.Context, projectID, agentID, claudeSessionID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sessions (project_id, agent_id, claude_session_id) VALUES ($1,$2,$3)
		 ON CONFLICT (claude_session_id) DO UPDATE SET status=sessions.status RETURNING id`,
		projectID, agentID, claudeSessionID).Scan(&id)
	return id, err
}

func (s *Store) SetSessionStatus(ctx context.Context, sessionID, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET status=$2 WHERE id=$1`, sessionID, status)
	return err
}

// ─────────────── tickets ───────────────

// OpenTicket devuelve el ticket activo a planear (último 'nuevo' o 'en_plan'); nil si no hay.
func (s *Store) OpenTicket(ctx context.Context, projectID string) (*Ticket, error) {
	var t Ticket
	var body *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, title, body, status FROM tickets
		 WHERE project_id=$1 AND status IN ('nuevo','en_plan','plan_espera')
		 ORDER BY created_at DESC LIMIT 1`, projectID).Scan(&t.ID, &t.Title, &body, &t.Status)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if body != nil {
		t.Body = *body
	}
	return &t, nil
}

func (s *Store) CreateTicket(ctx context.Context, projectID, title, body, source string) (string, error) {
	if title == "" {
		title = "Ticket sin título"
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tickets (project_id, title, body, source) VALUES ($1,$2,$3,$4) RETURNING id`,
		projectID, title, body, source).Scan(&id)
	return id, err
}

func (s *Store) SetTicketStatus(ctx context.Context, ticketID, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE tickets SET status=$2 WHERE id=$1`, ticketID, status)
	return err
}

// ReplaceTicketStages borra las fases previas del ticket y crea el plan nuevo (todas 'pendiente').
func (s *Store) ReplaceTicketStages(ctx context.Context, projectID, ticketID string, plan []PlanStage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM stages WHERE ticket_id=$1`, ticketID); err != nil {
		return err
	}
	for i, ps := range plan {
		if _, err := tx.Exec(ctx,
			`INSERT INTO stages (project_id, ticket_id, number, name, description, depends_on, status)
			 VALUES ($1,$2,$3,$4,$5,$6,'pendiente')`,
			projectID, ticketID, i+1, ps.Name, nullStr(ps.Description), ps.DependsOn); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ─────────────── plan (dinámico, por ticket) ───────────────

// LatestTicketID devuelve el ticket más reciente del proyecto (el que se muestra en Monitoreo).
func (s *Store) LatestTicketID(ctx context.Context, projectID string) (string, bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM tickets WHERE project_id=$1 ORDER BY created_at DESC LIMIT 1`, projectID).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	return id, err == nil, err
}

func (s *Store) StagesForTicket(ctx context.Context, ticketID string) ([]Stage, int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT number, name, status, description, depends_on FROM stages WHERE ticket_id=$1 ORDER BY number`, ticketID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Stage
	current := 1
	for rows.Next() {
		var st Stage
		if err := rows.Scan(&st.Number, &st.Name, &st.Status, &st.Description, &st.DependsOn); err != nil {
			return nil, 0, err
		}
		if st.Status == "activa" {
			current = st.Number
		}
		out = append(out, st)
	}
	return out, current, rows.Err()
}

// ─────────────── reportes ───────────────

// CreateReport crea un reporte (status 'espera'). ticketID y stageNumber son opcionales (0/"" = ninguno).
func (s *Store) CreateReport(ctx context.Context, projectID, agentID string, ticketID string, stageNumber int, title, summary string, deliverables, decisions []string) (string, error) {
	var stageID *string
	if ticketID != "" && stageNumber > 0 {
		var sid string
		if err := s.pool.QueryRow(ctx, `SELECT id FROM stages WHERE ticket_id=$1 AND number=$2`, ticketID, stageNumber).Scan(&sid); err == nil {
			stageID = &sid
		}
	}
	var tid *string
	if ticketID != "" {
		tid = &ticketID
	}
	del, _ := json.Marshal(deliverables)
	dec, _ := json.Marshal(decisions)
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO reports (project_id, agent_id, ticket_id, stage_id, title, status, summary, deliverables, decisions)
		 VALUES ($1,$2,$3,$4,$5,'espera',$6,$7::jsonb,$8::jsonb) RETURNING id`,
		projectID, agentID, tid, stageID, title, summary, string(del), string(dec)).Scan(&id)
	return id, err
}

// RespondReport aplica la decisión del jefe: estado, mensaje, reanuda sesión y avanza plan/ticket.
func (s *Store) RespondReport(ctx context.Context, reportID, action, message string) (claudeSessionID string, err error) {
	status := map[string]string{"aprobar": "aprobado", "cambios": "cambios", "responder": "respondido"}[action]
	if status == "" {
		return "", fmt.Errorf("acción inválida: %s", action)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var agentID, projectID string
	var stageID, ticketID *string
	if err := tx.QueryRow(ctx,
		`UPDATE reports SET status=$2 WHERE id=$1 RETURNING agent_id, project_id, stage_id, ticket_id`,
		reportID, status).Scan(&agentID, &projectID, &stageID, &ticketID); err != nil {
		return "", fmt.Errorf("update report: %w", err)
	}

	if action == "aprobar" {
		switch {
		case stageID != nil:
			// Reporte de una fase: esa fase 'hecha', la siguiente 'activa'.
			var num int
			var tid *string
			if err := tx.QueryRow(ctx, `UPDATE stages SET status='hecha', ended_at=now() WHERE id=$1 RETURNING number, ticket_id`, *stageID).Scan(&num, &tid); err == nil && tid != nil {
				_, _ = tx.Exec(ctx, `UPDATE stages SET status='activa', started_at=now() WHERE ticket_id=$1 AND number=$2 AND status='pendiente'`, *tid, num+1)
			}
		case ticketID != nil:
			// Reporte de PLAN: se aprueba el plan → ticket 'aprobado' y la 1ª fase 'activa'.
			_, _ = tx.Exec(ctx, `UPDATE tickets SET status='aprobado' WHERE id=$1`, *ticketID)
			_, _ = tx.Exec(ctx, `UPDATE stages SET status='activa', started_at=now() WHERE ticket_id=$1 AND number=1`, *ticketID)
		}
	} else if action == "cambios" && ticketID != nil && stageID == nil {
		// Plan rechazado: el ticket vuelve a planeación.
		_, _ = tx.Exec(ctx, `UPDATE tickets SET status='en_plan' WHERE id=$1`, *ticketID)
	}

	if message != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO messages (report_id, sender, body) VALUES ($1,'jefe',$2)`, reportID, message); err != nil {
			return "", fmt.Errorf("insert message: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `UPDATE sessions SET status='activa' WHERE agent_id=$1 AND status='pausada' RETURNING claude_session_id`, agentID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		_ = rows.Scan(&claudeSessionID)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return claudeSessionID, nil
}

func (s *Store) AgentMessage(ctx context.Context, reportID, body string) {
	if body == "" {
		return
	}
	_, _ = s.pool.Exec(ctx, `INSERT INTO messages (report_id, sender, body) VALUES ($1,'agente',$2)`, reportID, body)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
