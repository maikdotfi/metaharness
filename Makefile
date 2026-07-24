.PHONY: test test-integration

# Run the test suite with the race detector always on.
test:
	go test -race ./...

# Run live model provider tests (requires LLM_API_KEY or ANTHROPIC_API_KEY).
test-integration:
	go test -race -tags=integration ./model
