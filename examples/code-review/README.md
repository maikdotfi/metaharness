# Code Review Agent Demo

This is the smallest complete picture of Meta Harness today: an application
assembles a model, store, and tools into an `agent.Agent`, opens a sandbox
through a `sandbox.Manager`, and runs one mutable `agent.Session` — the task
bound to that sandbox — through the model/tool loop.

```mermaid
flowchart LR
    CLI[examples/code-review] --> Agent[agent.Agent]
    Config[model.Config] --> Model[model.New]
    Model --> Agent
    Discard[agent.DiscardStore] --> Agent
    OSTools["bash · read_file · edit_file · write_file"] --> Agent
    Skill["skill · grug-review"] --> Agent

    Sandboxes["sandbox.Manager\nsandbox.LocalBackend"] --> Box["Open(\"checkout\")"]
    Box --> Session[agent.Session]
    Session[agent.Session] --> Run[Agent.Run]
    Agent --> Run
    Run --> Events["assistant · tool_result · done · error"]

    Run --> Local["sandbox \"checkout\"\nworkdir = checkout/"]
    OSTools --> Local
    Local --> Checkout[checkout package]
```

## Assembly

The example owns the concrete dependencies; the core agent owns the loop.

```go
m, _ := model.New(model.Config{
    Provider: model.ProviderAnthropic,
    APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
})

a := agent.New(systemPrompt,
    agent.WithModel(m),
    agent.WithStore(agent.DiscardStore{}),
    agent.WithTools(
        agent.Adapt(tools.Bash{}),
        agent.Adapt(tools.ReadFile{}),
        agent.Adapt(tools.EditFile{}),
        agent.Adapt(tools.WriteFile{}),
        tools.NewSkill(skills.GrugReview()),
    ),
)
```

`agent.Adapt` derives each typed tool's JSON schema, exposes it to the model,
validates tool-call arguments, and then invokes the typed Go implementation.

```text
TypedTool[T] -> Adapt[T] -> agent.Tool -> model tool schema
                                      -> validated T -> Execute(...)
```

The skill is a normal tool. Calling `skill` puts the `grug-review`
instructions into the transcript; the next model turn follows them.

```text
skill({"skill":"grug-review"})
  -> <skill_content name="grug-review">...</skill_content>
  -> appended as a tool-result message
```

The agent holds no sandbox: model, tools, prompt and store are all it is, which
is why one agent serves every session.

## Session and run

The sandbox comes from the manager, and the session is the task bound to it. The
application owns the manager and closes it; the session owns its handle and
closes that.

```go
sandboxes, err := sandbox.New(sandbox.LocalKind, sandbox.WithRoot("."))
defer sandboxes.Close()

box, err := sandboxes.Open("checkout")
sess := agent.NewSession(fmt.Sprintf("review-%d", time.Now().Unix()), modelID, box)
defer sess.Close()
sess.Messages = append(sess.Messages, model.NewUserMessage(prompt))

events, err := a.Run(ctx, sess)
for event := range events {
    // Print assistant/tool activity, completion, or failure.
}
```

```mermaid
sequenceDiagram
    participant App as code-review
    participant Agent as agent.Run
    participant Model as FantasyModel / Anthropic
    participant Tool as selected tool
    participant Box as sandbox.Manager
    participant Store as DiscardStore

    App->>Box: Open("checkout")
    Box-->>App: sandbox handle
    App->>App: NewSession(id, model, handle)
    App->>Agent: Run(ctx, session)

    loop Until assistant returns without tool calls
        Agent->>Model: Generate(system prompt, transcript, tool schemas)
        Model-->>Agent: assistant text and/or tool calls + usage
        Agent->>Store: Save(session)
        opt Assistant requested tools
            Agent->>Tool: Execute(validated arguments, ExecCtx)
            opt Filesystem or shell tool
                Tool->>Box: Exec(command)
                Box-->>Tool: stdout, stderr, exit code
            end
            Tool-->>Agent: tool result
            Agent->>Agent: append result to transcript
            Agent->>Store: Save(session)
        end
    end

    Agent->>Agent: status = completed
    Agent->>Store: Save(session)
    Agent-->>App: EventDone
    App->>Box: session.Close()
```

A finished turn leaves the sandbox alone: the handle belongs to the session, and
the sandbox keeps running until the manager's idle policy or a `Destroy` says
otherwise.

`DiscardStore.Save` succeeds without retaining checkpoints, so the live
session is the only transcript. Applications can replace it with `JSONLStore`
through `agent.WithStore` when persistence is wanted.

```text
Session = ID + model ID + fantasy messages + token usage + status + its sandbox
```

The name is the sandbox's whole identity, and it is all that is persisted: image
and backend are this process's configuration, so a restored session is bound
again by name through `Session.Bind`. On `sandbox.LocalBackend` a name is a
directory under its root, and commands run there on the host. It only sets the process
working directory, and is deliberately a development backend rather than
security isolation: commands can reach the host and escape `workdir` with
absolute paths or `..`.

## Run it

From this folder:

```sh
export ANTHROPIC_API_URL=https://api.anthropic.com/
export ANTHROPIC_API_KEY=...
go run .
```

You can provide these flags to change the defaults without touching code:

```text
-model    Anthropic model id       (default: gemma4:31b-cloud)
-workdir  tool working directory   (default: checkout)
-prompt   initial user message     (there is a default one too)
```

The [`checkout/`](checkout/) package is intentionally imperfect and has its
own `go.mod`. Its failing quantity-validation test remains outside the root
test suite while giving the review agent concrete signal to find.
