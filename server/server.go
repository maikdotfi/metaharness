package server

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	gendb "github.com/maikdotfi/metaharness/db"
	sqlfs "github.com/maikdotfi/metaharness/sql"
)

const dbPath = "metaharness.db"

// Run is the entry point the CLI calls: it assembles the agent config from the
// supplied flag values and starts the HTTP server.
func Run(addr, agent, provider, model, baseURL string, mcp []string) error {
	return runServer(addr, AgentKind(agent), AgentConfig{
		Provider:   Provider(provider),
		Model:      model,
		BaseURL:    baseURL,
		MCPServers: ParseMCPSpecs(mcp),
	})
}

// ParseMCPSpecs turns each --mcp "cmd arg arg" string into a spec. Whitespace
// splitting is intentionally simple — these are operator-supplied commands, not
// arbitrary shell, so quoting/escaping is out of scope for this example.
func ParseMCPSpecs(raw []string) []MCPServerSpec {
	var specs []MCPServerSpec
	for _, r := range raw {
		fields := strings.Fields(r)
		if len(fields) == 0 {
			continue
		}
		specs = append(specs, MCPServerSpec{Command: fields[0], Args: fields[1:]})
	}
	return specs
}

func runServer(addr string, agentKind AgentKind, agentCfg AgentConfig) error {
	// Connect to SQLite (pure-Go driver, no CGO).
	pool, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Run goose migrations on startup from the embedded SQL files.
	goose.SetBaseFS(sqlfs.Migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	if err := goose.Up(pool, "migrations"); err != nil {
		return err
	}

	// Create the sqlc queries instance.
	queries := gendb.New(pool)

	// Sessions run synchronously against a fresh local machine each. The
	// provider/model and concrete agent come from flags.
	agent, err := NewAgent(agentKind, agentCfg)
	if err != nil {
		return err
	}

	// Set up the router and Huma API, then register every route in one place.
	router := http.NewServeMux()
	api := humago.New(router, huma.DefaultConfig("Meta Harness API", "0.1.0"))
	registerRoutes(api, queries, agent)

	slog.Info("listening", "addr", addr, "docs", fmt.Sprintf("http://localhost%s/docs", addr))
	if err := http.ListenAndServe(addr, router); err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// registerRoutes is the single place every HTTP route is declared, so the whole
// API surface can be read at a glance. Handlers themselves live in their own
// files by feature (e.g. sessions.go); this is just the route table wiring each
// method+path to its handler.
func registerRoutes(api huma.API, queries *gendb.Queries, agent Agent) {
	sessions := &sessionHandlers{queries: queries, agent: agent}
	huma.Post(api, "/sessions", sessions.Create)
	huma.Get(api, "/sessions/{id}", sessions.Get)
	huma.Get(api, "/sessions/{id}/events", sessions.ListEvents)
}

// Add new feature routes above by giving each its own handler struct in its own
// file and registering its methods here.
