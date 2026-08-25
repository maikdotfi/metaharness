// Command xmpp-chat assembles one personal Meta Harness agent and exposes it
// through XMPP. It logs in as a normal client account, answers the one owner it
// allows, and — with -digest-at — starts its own turn twice a day and reports
// what it found, unprompted.
//
// Run it from examples/xmpp-chat with the environment set:
//
//	export ANTHROPIC_API_KEY=sk-...
//	export XMPP_JID=agent@example.org
//	export XMPP_PASSWORD=...
//	export XMPP_OWNER=you@example.org
//	go run . -db ./sessions.db -digest-at 07:30,18:30
//
// -db is where persistence is chosen. With it, every turn is saved, /sessions
// and /resume work, and the agent keeps notes across sessions; without it
// nothing outlives the process. A digest starts a fresh session every time, so
// those notes are the only thing that carries: what the agent wants to know
// tomorrow it has to write down today.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/agentdb/turso"
	"github.com/maikdotfi/metaharness/bridge/xmpp"
	"github.com/maikdotfi/metaharness/memory"
	"github.com/maikdotfi/metaharness/model"
	"github.com/maikdotfi/metaharness/sandbox"
	"github.com/maikdotfi/metaharness/tools"
)

const systemPrompt = `You are a helpful personal assistant reachable over XMPP.

You have file and shell tools scoped to a working directory. Use them to inspect and change files and to run commands when a task calls for it. Keep replies concise and suitable for a chat window; when you show output, show only the relevant part.

Write down anything worth knowing next time, because your conversation does not survive the day and your notes do.`

// digestPrompt is what the bridge sends when a slot comes due. Silence is the
// library's only when there is no text at all, so the prompt has to ask for it:
// "nothing new today" is an answer, and it would be delivered.
const digestPrompt = `Nobody asked for this: your scheduled check-in is due.

Look at what has changed since your last one — your notes say what you were watching — and report only what is worth interrupting someone for. If nothing is, reply with nothing at all: no greeting, no "all quiet", an empty answer. Then write down what you looked at, so the next check-in starts where this one stopped.`

func main() {
	modelID := flag.String("model", "gemma4:31b-cloud", "Anthropic compatible model id")
	workdir := flag.String("workdir", "workspace", "directory the agent's tools run in")
	sandboxName := flag.String("sandbox", "default", "the named sandbox every session works in")
	dbPath := flag.String("db", "", "session database file; empty keeps sessions in memory only")
	digestAt := flag.String("digest-at", "", "times of day to report unprompted, e.g. 07:30,18:30; empty waits to be asked")
	digestContinues := flag.Bool("digest-continues", false,
		"let a digest continue the current conversation instead of replacing it")
	flag.Parse()

	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		log.Fatal("ANTHROPIC_API_KEY is not set")
	}
	owner := os.Getenv("XMPP_OWNER")
	if owner == "" {
		log.Fatal("XMPP_OWNER is not set: the bare JID allowed to talk to the agent")
	}

	// Cancelling reaches the XMPP stream and any in-flight turn.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	schedule, err := digestSchedule(*digestAt, *digestContinues)
	if err != nil {
		log.Fatalf("-digest-at: %v", err)
	}
	err = run(ctx, options{
		jid:         os.Getenv("XMPP_JID"),
		password:    os.Getenv("XMPP_PASSWORD"),
		owner:       owner,
		modelID:     *modelID,
		workdir:     *workdir,
		sandboxName: *sandboxName,
		dbPath:      *dbPath,
		schedule:    schedule,
	})
	if err != nil {
		log.Fatal(err)
	}
}

// options is what one assembled bridge needs. The JID and password are not
// checked here: Run reports a malformed one along with everything else.
type options struct {
	jid      string
	password string
	owner    string
	modelID  string

	workdir     string
	sandboxName string
	dbPath      string
	schedule    xmpp.Schedule
}

func run(ctx context.Context, opt options) error {
	m, err := model.New(model.Config{
		Provider: model.ProviderAnthropic,
		APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
		BaseURL:  os.Getenv("ANTHROPIC_API_URL"),
	})
	if err != nil {
		return err
	}

	// One manager for the process. Closing it releases the compute and leaves the
	// sandboxes for the next one.
	sandboxes, err := sandbox.New(sandbox.LocalKind, sandbox.WithRoot(opt.workdir))
	if err != nil {
		return err
	}
	defer sandboxes.Close()

	agentOptions := []agent.Option{
		agent.WithModel(m),
		agent.WithTools(
			agent.Adapt(tools.Bash{}),
			agent.Adapt(tools.ReadFile{}),
			agent.Adapt(tools.EditFile{}),
			agent.Adapt(tools.WriteFile{}),
		),
	}
	if opt.dbPath != "" {
		store, err := turso.Open(ctx, opt.dbPath)
		if err != nil {
			return err
		}
		defer store.Close()
		agentOptions = append(agentOptions,
			agent.WithStore(store),
			agent.WithMemory(memory.SystemPrompt(store)),
		)
	}
	a := agent.New(systemPrompt, agentOptions...)

	// The bridge starts the turns: the owner's messages, and the digest slots.
	return xmpp.Run(ctx, xmpp.Config{
		Username:    opt.jid,
		Password:    opt.password,
		Agent:       a,
		Sandboxes:   sandboxes,
		SandboxName: opt.sandboxName,
		Model:       opt.modelID,
		AllowedJIDs: []string{opt.owner},
		Schedule:    opt.schedule,
	})
}

// digestSchedule reads the times to report at. No times is the zero Schedule,
// which schedules nothing; a malformed one is reported by Run, with the time.
func digestSchedule(at string, continuing bool) (xmpp.Schedule, error) {
	var times []string
	for field := range strings.SplitSeq(at, ",") {
		if field = strings.TrimSpace(field); field != "" {
			times = append(times, field)
		}
	}
	if len(times) == 0 {
		if strings.TrimSpace(at) != "" {
			return xmpp.Schedule{}, errors.New("no times in it")
		}
		return xmpp.Schedule{}, nil
	}
	s := xmpp.Daily(digestPrompt, times...)
	if continuing {
		s = s.Continuing()
	}
	return s, nil
}
