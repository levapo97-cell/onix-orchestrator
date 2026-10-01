// Package store: acceso a Postgres para el plano de control (reports, sessions, stages, messages).
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

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

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

// Stage es una etapa del plan de 12.
type Stage struct {
	Number int    `json:"number"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// EnsureProject / Agent / Session (idempotentes), como en los otros servicios.
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

// SeedStages crea las 12 etapas del proyecto si no existen (1 = activa, resto pendiente).
func (s *Store) SeedStages(ctx context.Context, projectID string) error {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM stages WHERE project_id=$1`, projectID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for i := 1; i <= 12; i++ {
		status := "pendiente"
		if i == 1 {
			status = "activa"
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO stages (project_id, number, name, status) VALUES ($1,$2,$3,$4)
			 ON CONFLICT (project_id, number) DO NOTHING`,
			projectID, i, fmt.Sprintf("Etapa %d", i), status); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Stages(ctx context.Context, projectID string) ([]Stage, int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT number, name, status FROM stages WHERE project_id=$1 ORDER BY number`, projectID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Stage
	current := 1
	for rows.Next() {
		var st Stage
		if err := rows.Scan(&st.Number, &st.Name, &st.Status); err != nil {
			return nil, 0, err
		}
		if st.Status == "activa" {
			current = st.Number
		}
		out = append(out, st)
	}
	return out, current, rows.Err()
}

func (s *Store) stageID(ctx context.Context, projectID string, number int) (*string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM stages WHERE project_id=$1 AND number=$2`, projectID, number).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// CreateReport crea el reporte (status 'espera') y devuelve su id.
func (s *Store) CreateReport(ctx context.Context, projectID, agentID string, stageNumber int, title, summary string, deliverables, decisions []string) (string, error) {
	stageID, _ := s.stageID(ctx, projectID, stageNumber)
	del, _ := json.Marshal(deliverables)
	dec, _ := json.Marshal(decisions)
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO reports (project_id, agent_id, stage_id, title, status, summary, deliverables, decisions)
		 VALUES ($1,$2,$3,$4,'espera',$5,$6::jsonb,$7::jsonb) RETURNING id`,
		projectID, agentID, stageID, title, summary, string(del), string(dec)).Scan(&id)
	return id, err
}

// RespondReport aplica la decisión del jefe: actualiza estado, guarda el mensaje y reanuda la sesión.
// Devuelve el claude_session_id de la sesión asociada (para publicar en onix.ctrl.<sesión>).
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

	var agentID string
	if err := tx.QueryRow(ctx, `UPDATE reports SET status=$2 WHERE id=$1 RETURNING agent_id`, reportID, status).Scan(&agentID); err != nil {
		return "", fmt.Errorf("update report: %w", err)
	}
	if message != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO messages (report_id, sender, body) VALUES ($1,'jefe',$2)`, reportID, message); err != nil {
			return "", fmt.Errorf("insert message: %w", err)
		}
	}
	// Reanuda la(s) sesión(es) del agente que estaban pausadas.
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

// AgentMessage guarda el texto del agente (resumen/pregunta) como primer mensaje del chat.
func (s *Store) AgentMessage(ctx context.Context, reportID, body string) {
	if body == "" {
		return
	}
	_, _ = s.pool.Exec(ctx, `INSERT INTO messages (report_id, sender, body) VALUES ($1,'agente',$2)`, reportID, body)
}
