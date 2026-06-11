package cmd

import (
	"log/slog"
	"os"
	"strings"
)

// logLevelEnv selects the process log level: debug | info | warn | error.
// Anything else (including unset) means info, which keeps output pretty silent;
// debug turns on the raw agent instrumentation in the server package.
const logLevelEnv = "METAHARNESS_LOG_LEVEL"

// setupLogging installs the process-wide slog default: a text handler on stderr
// at the level chosen by METAHARNESS_LOG_LEVEL.
func setupLogging() {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv(logLevelEnv)) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}
