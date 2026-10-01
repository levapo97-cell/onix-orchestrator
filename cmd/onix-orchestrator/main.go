// Command onix-orchestrator — plano de control de OnixGuard (FASE 3).
//
// Expone un servidor MCP (reportar_al_jefe / pedir_al_jefe / consultar_plan) para los agentes.
// reportar_al_jefe y pedir_al_jefe son BLOQUEANTES: crean un reporte, pausan la sesión y la
// llamada MCP no retorna hasta que el jefe responde (desde el panel, vía gateway) o expira el timeout.
//
// Puertos: MCP en :8084 (/mcp), control+salud en :8085.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/nats-io/nats.go"

	"github.com/levapo97-cell/onix-orchestrator/internal/pending"
	"github.com/levapo97-cell/onix-orchestrator/internal/store"
)

var version = "dev"

type ctxKey string

const (
	ctxSession ctxKey = "onix-session"
	ctxProject ctxKey = "onix-project"
	ctxRole    ctxKey = "onix-role"
)

type app struct {
	st      *store.Store
	reg     *pending.Registry
	nc      *nats.Conn
	timeout time.Duration
}

func main() {
	// -healthcheck: para el HEALTHCHECK de Docker (imagen distroless, sin shell/curl).
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthcheck(env("PORT", "8085")))
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	mcpPort := env("MCP_PORT", "8084")
	ctrlPort := env("PORT", "8085")
	natsURL := env("NATS_URL", "nats://nats:4222")
	databaseURL := os.Getenv("DATABASE_URL")
	timeoutMin, _ := strconv.Atoi(env("REPORT_TIMEOUT_MIN", "15"))
	if databaseURL == "" {
		slog.Error("DATABASE_URL es obligatorio")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, databaseURL)
	if err != nil {
		slog.Error("postgres", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if err := st.WaitReady(ctx, 30*time.Second); err != nil {
		slog.Error("postgres no listo", "err", err)
		os.Exit(1)
	}
	nc, err := nats.Connect(natsURL, nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.Name("onix-orchestrator"))
	if err != nil {
		slog.Error("nats", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	a := &app{st: st, reg: pending.New(), nc: nc, timeout: time.Duration(timeoutMin) * time.Minute}

	// ─── Servidor MCP ───
	mcpSrv := server.NewMCPServer("onix-orchestrator", version)
	mcpSrv.AddTool(
		mcp.NewTool("reportar_al_jefe",
			mcp.WithDescription("Cierra una etapa: crea un reporte, pausa tu sesión y espera la decisión del jefe. Bloqueante."),
			mcp.WithNumber("stage", mcp.Required(), mcp.Description("Número de etapa (1..12) que reportas.")),
			mcp.WithString("summary", mcp.Required(), mcp.Description("Resumen de lo hecho.")),
			mcp.WithArray("deliverables", mcp.Description("Entregables con su ruta/artefacto.")),
			mcp.WithArray("decisions", mcp.Description("Decisiones que necesitan aprobación.")),
		), a.reportarAlJefe)
	mcpSrv.AddTool(
		mcp.NewTool("pedir_al_jefe",
			mcp.WithDescription("Pregunta bloqueante a mitad de etapa: espera la respuesta del jefe."),
			mcp.WithString("question", mcp.Required(), mcp.Description("Pregunta para el jefe.")),
		), a.pedirAlJefe)
	mcpSrv.AddTool(
		mcp.NewTool("consultar_plan",
			mcp.WithDescription("Devuelve el plan de 12 etapas y la etapa actual."),
		), a.consultarPlan)

	httpMCP := server.NewStreamableHTTPServer(mcpSrv,
		server.WithEndpointPath("/mcp"),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			ctx = context.WithValue(ctx, ctxSession, r.Header.Get("X-Onix-Session"))
			ctx = context.WithValue(ctx, ctxProject, r.Header.Get("X-Onix-Project"))
			ctx = context.WithValue(ctx, ctxRole, r.Header.Get("X-Onix-Role"))
			return ctx
		}),
	)
	go func() {
		slog.Info("MCP escuchando", "addr", ":"+mcpPort, "path", "/mcp")
		if err := httpMCP.Start(":" + mcpPort); err != nil && err != http.ErrServerClosed {
			slog.Error("mcp server", "err", err)
		}
	}()

	// ─── HTTP de control (gateway → orchestrator) + salud ───
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok", "service": "onix-orchestrator", "version": version})
	})
	mux.HandleFunc("POST /respond", a.handleRespond)
	ctrlSrv := &http.Server{Addr: ":" + ctrlPort, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("control escuchando", "addr", ":"+ctrlPort)
		if err := ctrlSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("control server", "err", err)
		}
	}()

	<-ctx.Done()
	slog.Info("apagando…")
	sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ctrlSrv.Shutdown(sc)
	_ = httpMCP.Shutdown(sc)
}

// ─────────────── Herramientas MCP ───────────────

func (a *app) identity(ctx context.Context) (session, project, role string) {
	session, _ = ctx.Value(ctxSession).(string)
	project, _ = ctx.Value(ctxProject).(string)
	role, _ = ctx.Value(ctxRole).(string)
	if session == "" {
		session = "sin-sesion"
	}
	if project == "" {
		project = "onixguard"
	}
	if role == "" {
		role = "project_lead"
	}
	return
}

func (a *app) reportarAlJefe(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	session, project, role := a.identity(ctx)
	args := req.GetArguments()
	stage := toInt(args["stage"])
	summary := toStr(args["summary"])
	deliverables := toStrSlice(args["deliverables"])
	decisions := toStrSlice(args["decisions"])

	reportID, err := a.openReport(ctx, project, role, session, stage, "Reporte de etapa "+strconv.Itoa(stage), summary, deliverables, decisions)
	if err != nil {
		return mcp.NewToolResultError("no se pudo crear el reporte: " + err.Error()), nil
	}
	resp := a.waitForBoss(ctx, reportID, session)
	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

func (a *app) pedirAlJefe(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	session, project, role := a.identity(ctx)
	question := toStr(req.GetArguments()["question"])

	reportID, err := a.openReport(ctx, project, role, session, 0, "Pregunta al jefe", question, nil, []string{question})
	if err != nil {
		return mcp.NewToolResultError("no se pudo crear la pregunta: " + err.Error()), nil
	}
	resp := a.waitForBoss(ctx, reportID, session)
	out, _ := json.Marshal(map[string]string{"message": resp.Message})
	return mcp.NewToolResultText(string(out)), nil
}

func (a *app) consultarPlan(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	_, project, _ := a.identity(ctx)
	pid, err := a.st.EnsureProject(ctx, project)
	if err == nil {
		_ = a.st.SeedStages(ctx, pid)
	}
	stages, current, err := a.st.Stages(ctx, pid)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out, _ := json.Marshal(map[string]any{"current_stage": current, "stages": stages})
	return mcp.NewToolResultText(string(out)), nil
}

// openReport hace el alta en DB y pausa la sesión. Devuelve el report_id.
func (a *app) openReport(ctx context.Context, project, role, session string, stage int, title, summary string, deliverables, decisions []string) (string, error) {
	pid, err := a.st.EnsureProject(ctx, project)
	if err != nil {
		return "", err
	}
	_ = a.st.SeedStages(ctx, pid)
	aid, err := a.st.EnsureAgent(ctx, pid, role)
	if err != nil {
		return "", err
	}
	sid, err := a.st.EnsureSession(ctx, pid, aid, session)
	if err != nil {
		return "", err
	}
	reportID, err := a.st.CreateReport(ctx, pid, aid, stage, title, summary, deliverables, decisions)
	if err != nil {
		return "", err
	}
	a.st.AgentMessage(ctx, reportID, summary)
	if err := a.st.SetSessionStatus(ctx, sid, "pausada"); err != nil {
		slog.Warn("no se pudo pausar la sesión", "err", err)
	}
	// Notifica al panel (vía gateway) que llegó un reporte nuevo.
	payload, _ := json.Marshal(map[string]string{"report_id": reportID, "project": project})
	_ = a.nc.Publish("onix.report."+project, payload)
	slog.Info("reporte creado · sesión pausada", "report_id", reportID, "session", session, "stage", stage)
	return reportID, nil
}

// waitForBoss bloquea hasta la respuesta del jefe o el timeout (no cuelga para siempre).
func (a *app) waitForBoss(ctx context.Context, reportID, session string) pending.Response {
	ch := a.reg.Register(reportID)
	select {
	case resp := <-ch:
		slog.Info("reanudado por el jefe", "report_id", reportID, "action", resp.Action)
		return resp
	case <-time.After(a.timeout):
		a.reg.Cancel(reportID)
		slog.Warn("timeout esperando al jefe", "report_id", reportID)
		return pending.Response{Action: "timeout", Message: "Sin respuesta del jefe todavía; el reporte sigue en espera."}
	case <-ctx.Done():
		a.reg.Cancel(reportID)
		return pending.Response{Action: "timeout", Message: "conexión cerrada"}
	}
}

// ─────────────── Control: respuesta del jefe (desde el gateway) ───────────────

func (a *app) handleRespond(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ReportID string `json:"report_id"`
		Action   string `json:"action"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ReportID == "" || body.Action == "" {
		writeJSON(w, 400, map[string]any{"error": "report_id y action son obligatorios"})
		return
	}
	// Persiste la decisión y reanuda la sesión.
	claudeSession, err := a.st.RespondReport(r.Context(), body.ReportID, body.Action, body.Message)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	// Publica en onix.ctrl.<sesión> (auditoría + futuro onix-agent).
	if claudeSession != "" {
		ctrl, _ := json.Marshal(map[string]string{"action": body.Action, "message": body.Message})
		_ = a.nc.Publish("onix.ctrl."+claudeSession, ctrl)
	}
	// Retorna la llamada MCP pendiente (si sigue viva en esta instancia).
	resolved := a.reg.Resolve(body.ReportID, pending.Response{Action: body.Action, Message: body.Message})
	writeJSON(w, 200, map[string]any{"ok": true, "resolved": resolved})
}

// ─────────────── helpers ───────────────

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runHealthcheck(port string) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func toStrSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
