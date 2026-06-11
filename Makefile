DB_PATH ?= metaharness.db

.PHONY: generate migrate-up migrate-down run tools

# Install the dev tooling (sqlc + goose) into your GOBIN.
tools:
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
	go install github.com/pressly/goose/v3/cmd/goose@latest

generate:
	sqlc generate

migrate-up:
	goose -dir sql/migrations sqlite3 "$(DB_PATH)" up

migrate-down:
	goose -dir sql/migrations sqlite3 "$(DB_PATH)" down

run: generate
	go run . serve
