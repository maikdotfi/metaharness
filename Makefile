.PHONY: test test-integration test-docker test-lightpanda

# Run the test suite with the race detector always on.
test:
	go test -race ./...

# Run live model provider tests (requires LLM_API_KEY or ANTHROPIC_API_KEY).
test-integration:
	go test -race -tags=integration ./model

# Run the sandbox tests against a real Docker daemon (requires one running).
test-docker:
	go test -race -tags=docker ./sandbox/docker

# Run the MCP tests against a real `lightpanda mcp` server (requires lightpanda
# on PATH).
test-lightpanda:
	go test -race -tags=lightpanda ./mcp/lightpanda
