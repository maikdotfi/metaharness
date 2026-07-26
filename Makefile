.PHONY: test test-integration test-docker lint-determinism

# Run the test suite with the race detector always on. Nothing in it needs Docker,
# a network, or the wall clock.
test: lint-determinism
	go test -race ./...

# Run live model provider tests (requires LLM_API_KEY or ANTHROPIC_API_KEY).
test-integration:
	go test -race -tags=integration ./model

# Run the sandbox tests that drive a real Docker daemon.
test-docker:
	go test -race -tags=docker -run Docker ./agent

# Sleep policy is only testable if nothing reaches the wall clock or randomness
# behind a test's back: time has to arrive through agent.Clock, which a fake clock
# can then advance. agent/clock.go is the systemClock adapter, so it is the one
# legitimate caller of the time package's clock here.
FORBIDDEN := time\.(Now|After|AfterFunc|Sleep|Tick|NewTicker|NewTimer)\(|math/rand
lint-determinism:
	@hits=$$(grep -REn '$(FORBIDDEN)' --include='*.go' agent sandbox | grep -v '^agent/clock\.go:' || true); \
	if [ -n "$$hits" ]; then \
		echo "forbidden direct clock or randomness use; inject agent.Clock instead:"; \
		echo "$$hits"; \
		exit 1; \
	fi
	@echo "determinism: ok"
