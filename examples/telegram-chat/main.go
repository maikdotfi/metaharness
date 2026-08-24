// Command telegram-chat assembles one personal Meta Harness agent and exposes it
// through a private Telegram chat. It talks to Telegram with long polling only —
// there is no HTTP listener — and talks to one session at a time, which /new
// resets.
//
// Run it from examples/telegram-chat with the environment set:
//
//	export ANTHROPIC_API_KEY=sk-...
//	export TELEGRAM_BOT_TOKEN=123456:ABC...        # from @BotFather
//	export TELEGRAM_ALLOWED_USERS=11111111         # your numeric Telegram id(s)
//	export METAHARNESS_SANDBOX=work                # which sandbox to work in
//	go run . -workdir ./workspace -db ./sessions.db
//
// METAHARNESS_SANDBOX names the sandbox to work in, creating it if it does not
// exist yet. The same name is the same filesystem across turns, sessions and
// restarts, and it survives until it is destroyed explicitly.
//
// The agent also gets browser tools, from a `lightpanda mcp` server run beside
// it. Nothing starts until a tool is called, so lightpanda is only needed if the
// model reaches for the browser.
//
// -db is where persistence is chosen. With it, every turn is saved and /sessions
// and /resume work; without it the agent keeps the default DiscardStore and the
// live session is the only transcript.
//
// See README.md for BotFather setup and how to find your numeric user id.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/agentdb/turso"
	"github.com/maikdotfi/metaharness/bridge/telegram"
	"github.com/maikdotfi/metaharness/mcp"
	"github.com/maikdotfi/metaharness/mcp/lightpanda"
	"github.com/maikdotfi/metaharness/model"
	"github.com/maikdotfi/metaharness/sandbox"
	"github.com/maikdotfi/metaharness/tools"

	// Importing a backend registers its -sandbox kind. This line is all this
	// program says about Docker: delete it and only the local backend remains.
	_ "github.com/maikdotfi/metaharness/sandbox/docker"
)

const systemPrompt = `You are a helpful personal assistant reachable over Telegram.

You have file and shell tools scoped to a working directory. Use them to inspect and change files and to run commands when a task calls for it. You also have browser tools: load a page with browser_goto, then read it with browser_markdown or browser_links. Keep replies concise and suitable for a chat window; when you show output, show only the relevant part.`

// defaultSandboxName is where work goes when METAHARNESS_SANDBOX says nothing:
// every sandbox has a name, and a stable default beats a throwaway one.
const defaultSandboxName = "default"

func main() {
	modelID := flag.String("model", "gemma4:31b-cloud", "Anthropic compatible model id")
	workdir := flag.String("workdir", "workspace", "directory the agent's tools run in")
	showThinking := flag.Bool("show-thinking", false, "include the model's reasoning text in the progress status message")
	think := flag.Bool("think", false, "enable extended thinking output")
	effort := flag.String("effort", "medium", "thinking effort (low, medium, high, xhigh, max); only used with -think")
	// The help text lists the backends this binary actually linked in.
	sandboxKind := flag.String("sandbox", sandbox.LocalKind,
		"where sandboxes live: "+strings.Join(sandbox.Backends(), ", "))
	image := flag.String("image", "golang:1.26", "container image to work in; ignored by the local backend")
	dbPath := flag.String("db", "", "session database file; empty keeps sessions in memory only")
	flag.Parse()

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Fatal("ANTHROPIC_API_KEY is not set")
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is not set")
	}
	allowed, err := parseAllowedUsers(os.Getenv("TELEGRAM_ALLOWED_USERS"))
	if err != nil {
		log.Fatalf("TELEGRAM_ALLOWED_USERS: %v", err)
	}

	// Cancelling reaches both the Telegram long poll and any in-flight turn.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = run(ctx, options{
		token:        token,
		modelID:      *modelID,
		sandboxKind:  *sandboxKind,
		workdir:      *workdir,
		sandboxName:  sandboxName(),
		image:        *image,
		dbPath:       *dbPath,
		allowed:      allowed,
		showThinking: *showThinking,
		think:        *think,
		effort:       *effort,
	})
	if err != nil {
		log.Fatal(err)
	}
}

// options is what one assembled bridge needs.
type options struct {
	token   string
	modelID string

	// sandboxKind is where sandboxes live and workdir where the ones on the host
	// filesystem go; sandboxName is the one every session works in, and image what
	// to create it from if it does not exist yet.
	sandboxKind string
	workdir     string
	sandboxName string
	image       string

	// dbPath is where sessions are stored, or empty for no persistence at all.
	dbPath       string
	allowed      []int64
	showThinking bool
	think        bool
	effort       string
}

// logSandboxEvent reports the lifecycle changes that have no caller to return to:
// an idle stop, the container the next command creates or wakes, and a backend
// that could not be reached at all — which names no sandbox.
func logSandboxEvent(ev sandbox.Event) {
	if ev.Err != nil {
		slog.Warn("sandbox "+ev.Type.String(), "sandbox", ev.Name, "state", ev.To, "err", ev.Err)
		return
	}
	slog.Info("sandbox "+ev.Type.String(), "sandbox", ev.Name, "state", ev.To)
}

// sandboxName reads which sandbox to work in from the environment. That choice is
// the application's, never the library's.
func sandboxName() string {
	if name := os.Getenv("METAHARNESS_SANDBOX"); name != "" {
		return name
	}
	return defaultSandboxName
}

func run(ctx context.Context, opt options) error {
	cfg := model.Config{
		Provider: model.ProviderAnthropic,
		APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
		BaseURL:  os.Getenv("ANTHROPIC_API_URL"),
	}
	if opt.think {
		cfg.Thinking = &model.Thinking{Effort: model.Effort(opt.effort)}
	}
	m, err := model.New(cfg)
	if err != nil {
		return err
	}

	// One manager for the process: sandboxes are shared across turns, and their
	// compute is released once nobody has run anything for a while. Closing it is
	// the only cleanup here — it releases the goroutines, the idle timers and the
	// backend's connection, and leaves the sandboxes for the next process.
	sandboxManager, err := sandbox.New(opt.sandboxKind,
		sandbox.WithRoot(opt.workdir),
		sandbox.WithImage(opt.image),
		sandbox.WithObserver(logSandboxEvent),
	)
	if err != nil {
		return err
	}
	defer sandboxManager.Close()

	// A browser, from a server that is a value: nothing is dialed here, so this
	// costs nothing until a tool is called and needs lightpanda on PATH only then.
	// Missing it is a tool error the model reads and stops asking for.
	browser := mcp.Stdio("lightpanda", []string{"mcp"})
	defer browser.Close()

	// Persistence is a choice, and this is where it is made: with -db the agent
	// saves every turn to a local Turso database and /resume can bring a session
	// back, without it DiscardStore keeps everything in memory. Linking the store
	// is what brings the database driver in, so the storage-free build has none.
	agentOptions := []agent.Option{
		agent.WithModel(m),
		agent.WithTools(
			agent.Adapt(tools.Bash{}),
			agent.Adapt(tools.ReadFile{}),
			agent.Adapt(tools.EditFile{}),
			agent.Adapt(tools.WriteFile{}),
		),
		agent.WithTools(lightpanda.Tools(browser)...),
	}
	if opt.dbPath != "" {
		store, err := turso.Open(ctx, opt.dbPath)
		if err != nil {
			return err
		}
		defer store.Close()
		agentOptions = append(agentOptions, agent.WithStore(store))
	}

	// The agent holds no sandbox, which is why one can serve every session.
	a := agent.New(systemPrompt, agentOptions...)

	// The bridge starts the tasks: /new is a Telegram command, and every task it
	// starts opens this same named sandbox again, so a reset discards the
	// conversation and keeps the files. Without -db the agent retains nothing, and
	// the bridge offers no /sessions or /resume.
	return telegram.Run(ctx, telegram.Config{
		Token:        opt.token,
		Agent:        a,
		Sandboxes:    sandboxManager,
		SandboxName:  opt.sandboxName,
		Model:        opt.modelID,
		AllowedUsers: opt.allowed,
		ShowThinking: opt.showThinking,
	})
}

// parseAllowedUsers reads a comma-separated list of numeric Telegram user ids.
// It requires at least one so the bridge is never open to everyone.
func parseAllowedUsers(raw string) ([]int64, error) {
	var ids []int64
	for field := range strings.SplitSeq(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		id, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New(
			"set it to your numeric Telegram user id (comma-separated for more than one)",
		)
	}
	return ids, nil
}
