# Code Review Agent Demo

This directory is a small review target for the `code-review` agent. The sample
checkout package is intentionally imperfect so the agent has concrete things to
find.

From the repository root, run:

```sh
go run . agent \
  --agent code-review \
  --workdir ./demo/code-review \
  --prompt "Review this checkout package. Inspect the code and tests, run anything useful, and return review feedback with file and line references."
```

By default this uses the Anthropic provider, so set `ANTHROPIC_API_KEY` first.
For a local Ollama-compatible run:

```sh
go run . agent \
  --agent code-review \
  --provider ollama \
  --model qwen2.5-coder \
  --workdir ./demo/code-review \
  --prompt "Review this checkout package. Inspect the code and tests, run anything useful, and return review feedback with file and line references."
```

The exact response depends on the model, but useful feedback should mention the
missing quantity validation, weak coupon validation, and checkout behavior.
Running `go test ./...` in this directory currently fails on the quantity
validation case; that is intentional review signal for the demo.
