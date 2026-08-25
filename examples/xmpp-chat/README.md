# xmpp-chat

A personal XMPP bridge for one assembled Meta Harness agent, and the example of
an agent that starts its own turns. It logs in as an ordinary client account —
no component, no server-side setup beyond having an account — and only the one
JID in `XMPP_OWNER` may talk to it. The agent has file and shell tools, so that
allowlist is the whole security model.

## Run it

```sh
export ANTHROPIC_API_KEY=sk-ant-...
export XMPP_JID=agent@example.org        # the account the agent logs in as
export XMPP_PASSWORD=...
export XMPP_OWNER=you@example.org        # the only person it answers

cd examples/xmpp-chat
go run . -db ./sessions.db -digest-at 07:30,18:30
```

Then message the agent from any XMPP client. `-workdir` is where the sandbox's
files live, `-db` keeps sessions and notes across restarts, and `-model` picks
the model id.

## The digest

`-digest-at` is the point of this example. At each time of day it lists, the
bridge starts a turn nobody typed, and sends what comes of it to your bare JID —
so it reaches whichever client is connected, or waits offline until one is.
A scheduled turn is quiet: no progress trail, and nothing at all when the agent
produces no text.

Each digest starts a fresh session, so the conversation that follows one is
about that digest. What carries across them is the agent's notes, not its
transcript, which is why `-db` matters more here than in a chat you drive
yourself. Pass `-digest-continues` to keep one growing conversation instead;
that is the knob to experiment with, not a setting with a known right answer.

"Nothing new" is the prompt's job, not the bridge's. The bridge stays silent
only when the agent produces no text at all — a model that answers "all quiet!"
has produced text, and you will get it. The digest prompt in `main.go` asks for
an empty answer explicitly.

Times are `HH:MM` in the process's local zone, there are no cron expressions,
and a slot missed while the process was down is not made up. The clock says when
to look; what is actually outstanding is your own data's business.
