// Command telegram-chat assembles one personal Meta Harness agent and exposes it
// through a private Telegram chat. It talks to Telegram with long polling only —
// there is no HTTP listener — and keeps a single in-memory session that you can
// reset with /new.
//
// Run it from examples/telegram-chat with the environment set:
//
//	export ANTHROPIC_API_KEY=sk-...
//	export TELEGRAM_BOT_TOKEN=123456:ABC...        # from @BotFather
//	export TELEGRAM_ALLOWED_USERS=11111111         # your numeric Telegram id(s)
//	export METAHARNESS_SANDBOX=work                # which sandbox to work in
//	go run . -workdir ./workspace
//
// METAHARNESS_SANDBOX names the sandbox to work in, creating it if it does not
// exist yet. The same name is the same filesystem across turns, sessions and
// restarts, and it survives until it is destroyed explicitly.
//
// See README.md for BotFather setup and how to find your numeric user id.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/maikdotfi/metaharness/bridge/telegram"
	"github.com/maikdotfi/metaharness/model"
	"github.com/maikdotfi/metaharness/sandbox"
	"github.com/maikdotfi/metaharness/tools"

	// Importing a backend registers its -sandbox kind. This line is all this
	// program says about Docker: delete it and only the local backend remains.
	_ "github.com/maikdotfi/metaharness/sandbox/docker"
)

const systemPrompt = `You are a helpful personal assistant reachable over Telegram.

You have file and shell tools scoped to a working directory. Use them to inspect and change files and to run commands when a task calls for it. Keep replies concise and suitable for a chat window; when you show output, show only the relevant part.`

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
	sandboxKind  string
	workdir      string
	sandboxName  string
	image        string
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

	// The agent holds no sandbox, which is why one can serve every session.
	a := agent.New(systemPrompt,
		agent.WithModel(m),
		// DiscardStore is the default; the personal bridge is storage-free.
		agent.WithTools(
			agent.Adapt(tools.Bash{}),
			agent.Adapt(tools.ReadFile{}),
			agent.Adapt(tools.EditFile{}),
			agent.Adapt(tools.WriteFile{}),
		),
	)

	// Every task gets a new id and opens the same named sandbox again, so /new
	// discards the conversation and keeps the files.
	newSession := func() (*agent.Session, error) {
		box, err := sandboxManager.Open(opt.sandboxName)
		if err != nil {
			return nil, err
		}
		return agent.NewSession(newSessionID(), opt.modelID, box), nil
	}

	return telegram.Run(ctx, telegram.Config{
		Token:        opt.token,
		Agent:        a,
		NewSession:   newSession,
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

// newSessionID returns an opaque local identifier. It deliberately encodes no
// Telegram bot, chat, or user id.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A readable panic beats a predictable id.
		panic("telegram-chat: reading random bytes: " + err.Error())
	}
	return "sess_" + hex.EncodeToString(b[:])
}
