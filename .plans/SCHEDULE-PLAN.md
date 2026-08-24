# Scheduled Turns Plan

## Direction

Every turn a bridge has ever run started with somebody typing. A podcast agent
that reports twice a day starts its own, and that is the whole gap: the XMPP
bridge's loop selects on cancellation, the stream ending, and an incoming
message (`bridge/xmpp/bridge.go`), and a scheduled digest is a fourth case.

The bridge is already the right owner. `docs/api-design.md` move 8 established
that starting a task is the bridge's own work, not a callback the application
supplies — `/new` proved it for a typed command, and a timer is the same
argument with no typing. What the application knows is two things: when, and
what to ask. The bridge assembles the rest and decides where the answer goes.

Keep the schedule in `bridge/xmpp`. `BRIDGE-PLAN.md` refused a generic transport
interface until a second bridge gave a concrete comparison, and the same refusal
applies here: Telegram may want this, and when it does, the type moves and the
two call sites tell us what it should have looked like.

## Goals

- Let an assembled agent start its own turn at fixed times of day, in a
  long-lived process, with no external cron.
- Deliver the result to the user unprompted, and have their reply land in the
  same conversation.
- Keep the session policy a knob, so the right answer can be learned from use
  rather than guessed now.
- Keep a scheduled run quiet: no progress trail, and nothing sent when there is
  nothing to say.
- Serialize scheduled turns against typed ones, using the loop that already
  serializes everything.

## Non-Goals

- No cron expressions. Times of day, in the process's local zone.
- No catch-up for slots missed while the process was down.
- No durable record of what fired. Whether work is outstanding is the
  application's ledger, not the bridge's clock — see "The clock is not the
  ledger".
- No per-recipient schedules. One personal agent, one schedule.
- No cancellation of a scheduled turn in flight.
- No generic bridge or scheduler interface.

## The caller

```go
return xmpp.Run(ctx, xmpp.Config{
	Username:    opt.jid,
	Password:    opt.password,
	Agent:       a,
	Sandboxes:   sandboxManager,
	SandboxName: opt.sandboxName,
	Model:       opt.modelID,
	AllowedJIDs: []string{opt.owner},
	Schedule:    xmpp.Daily(digestPrompt, "07:30", "18:30"),
})
```

One line. The experiment knob is one call further:

```go
	Schedule: xmpp.Daily(digestPrompt, "07:30", "18:30").Continuing(),
```

## Public Shape

```go
// package xmpp

// Schedule is a prompt the bridge starts on its own. The zero value schedules
// nothing.
type Schedule struct{ /* unexported */ }

// Daily runs prompt at each given time of day, in the process's local time
// zone. A time is "HH:MM"; an invalid one is reported when Run validates its
// configuration, alongside a bad JID.
//
// Each run starts a fresh session, so the conversation that follows a digest is
// about that digest. What the agent knows across those sessions is its memory,
// not its transcript.
func Daily(prompt string, at ...string) Schedule

// Continuing returns a schedule whose runs continue the current session instead
// of replacing it. The conversation then never resets on its own, and grows
// until the user sends /new.
func (s Schedule) Continuing() Schedule
```

`Config` gains one field, `Schedule Schedule`.

`Daily` takes the prompt and the times as arguments because forgetting either is
a bug the compiler can catch — a `Schedule` with no times is not a schedule.
`Continuing` is a method rather than a second constructor or a `SessionPolicy`
field so that the mandatory parts stay in the signature and the optional part
reads as the exception it is. If a third policy appears — "fresh only if the
conversation has been idle an hour" is the likely one — `Continuing` becomes
`WithSession(policy)`, a one-line change at one call site. That is the trade,
named: today's shape is smaller and does not survive a third answer.

## Session lifecycle

The decision, to be revisited from real use:

- A scheduled run **starts a fresh session** and makes it current, closing the
  previous one the way `/new` does. Same named sandbox, so the files, the
  database and the ledger carry over — only the conversation resets.
- The user's replies continue that session, for as long as they like.
- The next scheduled run ends it. A reply to yesterday morning's digest, sent
  after last night's, lands in the current session and reads as a non sequitur;
  memory is what keeps that from mattering, because anything worth keeping was
  written down rather than left in the transcript.

No "started a fresh session" announcement. The digest is the announcement, and a
second message saying so twice a day is noise. `/status` shows the new id for
anyone who cares.

`Continuing` exists because that argument may turn out to be wrong in practice —
if the interesting conversation is usually the one interrupted, the reset is a
misfeature and the growth is a price worth paying.

## What a scheduled turn sends

Three differences from a typed turn, all deliberate:

- **To the bare JID**, for every allowed JID. Every reply path today answers
  `msg.From`, a full JID with a resource, because there was an incoming stanza
  to answer. A scheduled run has none, and a bare JID is what the server routes
  to whichever client is connected — or stores offline until one is. Sending to
  a remembered resource would deliver to a laptop that closed at 18:00.
  Recipient is not a schedule field: everyone allowed to talk to a personal
  agent is a person it belongs to, and a fourth place to write a JID is a fourth
  place to write it wrong.
- **No progress trail.** The status message and its XEP-0308 corrections exist
  because a human is watching a prompt they just sent. At 07:30 that trail is a
  notification for work nobody asked to watch yet.
- **Silence when there is nothing to say.** A typed prompt that produces no text
  gets "(the agent finished without any text to show)", which is right — someone
  is waiting. A scheduled one sends nothing at all.

The bridge can only suppress genuinely empty text. "No change is not news" is
mostly the prompt's job, and the application's digest prompt has to ask for
silence explicitly; a model that answers "Nothing new today!" has produced text
and it will be sent. Worth stating in the example's prompt and worth not
pretending the library solves.

## Firing

A pure function does the arithmetic and takes the tests:

```go
func nextDue(now time.Time, at []string) time.Time
```

Built with `time.Date(..., time.Local)` so wall-clock times survive a DST
change, advancing a day when today's slot has passed. Duplicates and unsorted
times are the caller's to write and this function's to tolerate.

The loop then selects on a timer for `nextDue(now).Sub(now)` alongside the three
existing cases. Two unexported seams, matching `now` and `correctionGap` which
are already there for the same reason: a clock and an `after` the tests replace.
Nothing new is exported to make this testable.

**At most once per slot, per process, and no catch-up.** A process that starts
at 09:00 does not run the 07:30 slot; the 18:30 one fires normally. A digest is
time-relevant, catch-up needs durable last-fired state the bridge does not have,
and the cost of a missed slot is one late report — the application's ledger
still knows the episodes are unprocessed, so nothing is lost, only delayed.

### The clock is not the ledger

The bridge's timer says *when to look*, never *what is outstanding*. That
distinction is why no fired-slot table is needed: a scheduled run that
double-fires across a restart finds the same answer twice from a store that
already recorded the work, and reports nothing the second time. Everything that
makes a re-run cheap — idempotent writes, dedup, caps on how much work one run
can start — lives in the application's tools, tested, rather than in the prompt.

## Replies that arrive late

`decodeMessage` drops any stanza carrying a `<delay/>` — a message the server
queued while the bridge was offline. For a bridge that only reacts, that is the
right stale-update policy and it matches Telegram's. For an agent whose whole
point is that the user answers an unprompted digest, it now throws away exactly
the message that matters: the reply typed at 08:00 to a bridge that restarted at
08:05.

Change: accept a delayed chat message whose stamp is younger than
`maxOfflineAge` (unexported, 12h — one digest interval), drop and log older
ones. It is a constant rather than configuration because one number that can be
changed in one place beats an option nobody knows how to set; if 12h is wrong,
the fix is one line.

This is a behaviour change to an existing policy, so it gets its own failing
test and it can be dropped from the rollout independently — with the process
genuinely long-lived, delayed messages are rare.

## Turn serialization

Nothing new is needed. `Run`'s loop is single-threaded: it handles one incoming
message to completion before selecting again, so a slot that comes due during a
turn is served when that turn ends. The existing `b.mu` continues to guard
`b.current` against the deferred close on shutdown. A scheduled turn takes the
same path as a typed one — `handlePrompt` with a synthesized prompt — which is
what keeps the two from needing separate reasoning about the session.

## Testing

Failing test first, against the existing `fakeAPI` and fake model in
`bridge/xmpp/bridge_test.go`.

`nextDue`, table-driven and pure:

- a time later today; a time already passed today rolls to tomorrow; the only
  time being exactly now; unsorted and duplicate times; a slot inside a
  spring-forward gap.

The bridge, with a fake clock:

- a due slot runs the schedule's prompt without any incoming message, and the
  final text is sent to the **bare** allowed JID.
- a scheduled turn sends no progress message — exactly one stanza for a run that
  emits several events.
- a scheduled turn whose final text is empty sends nothing at all.
- `Daily` replaces the current session; the session id after a run differs from
  before, and the sandbox name does not.
- `.Continuing()` keeps the session id, and the transcript carries both turns.
- a slot that comes due while a typed turn is in flight runs after it, and the
  two do not interleave.
- a schedule with a malformed time fails `Config.validate` with the time named,
  and `Run` returns before connecting.
- an empty `Schedule` leaves every existing behaviour identical — the regression
  guard.
- a delayed message younger than `maxOfflineAge` is handled; an older one is
  ignored.

## Rollout Steps

1. `nextDue` and its table test. No bridge changes yet.
2. `Schedule`, `Daily`, `Continuing`, the `Config` field, and validation of the
   times alongside the JIDs.
3. The fourth select case, the fresh-session behaviour, and the three send
   differences. Scheduled turns go through `handlePrompt` with a flag for the
   quiet path rather than a parallel implementation.
4. The delayed-message policy change, separately, so it can be reverted alone.
5. `examples/podcast/main.go`: the assembly, the digest prompt, and the
   application's own ledger tools. This is where `MEMORY-PLAN.md` and this plan
   meet, and where the claim that memory is what makes a disposable session
   affordable either holds or does not.
6. Update the bridge's `/help` — a user who did not write the config should be
   able to find out the agent wakes up on its own — plus `STACK.md` and the
   example's README.

## Later, deliberately not now

- **Hoisting `Schedule` out of `bridge/xmpp`.** When Telegram wants it. Two call
  sites are the comparison `BRIDGE-PLAN.md` asked for.
- **Catch-up after downtime.** Needs durable last-fired state, and the ledger
  makes it mostly unnecessary. Revisit if a missed morning is ever actually felt.
- **A third session policy.** `Continuing` becomes `WithSession(policy)` then,
  not before.
- **Cancelling a scheduled turn.** `BRIDGE-PLAN.md` already deferred cancelling a
  typed one; a scheduled turn is not the case that changes the answer.
- **Cron expressions, intervals, one-off runs.** "Twice a day" is what exists.
