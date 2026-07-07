.PHONY: test

# Run the test suite with the race detector always on.
test:
	go test -race ./...
