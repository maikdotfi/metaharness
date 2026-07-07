package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/maikdotfi/metaharness/agent"
)

// The file tools reach the filesystem the only way a tool can: through the
// sandbox's shell. These helpers keep the shell plumbing in one place so
// read_file, write_file, and edit_file all quote paths and shuttle bytes the
// same way.

// shellQuote wraps s in single quotes so it can be embedded verbatim in a bash
// command line, whatever characters it contains. A literal single quote is
// closed, escaped, and reopened ('\'') — the standard POSIX trick.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runBash executes script through the sandbox's bash, mirroring how the bash
// tool itself runs commands.
func runBash(ctx context.Context, ec *agent.ExecCtx, script string) (agent.ExecResult, error) {
	return ec.Sandbox.Exec(ctx, agent.Command{Cmd: "bash", Args: []string{"-c", script}})
}

// catFile reads path from the sandbox. A non-zero ExitCode (e.g. the file is
// missing) is reported through the result, not the error — the error return is
// reserved for infra failures that should abort the run.
func catFile(ctx context.Context, ec *agent.ExecCtx, path string) (agent.ExecResult, error) {
	return runBash(ctx, ec, "cat -- "+shellQuote(path))
}

// putFile writes content to path in the sandbox, creating any missing parent
// directories. content is base64-encoded on the way in and decoded in the
// sandbox so that arbitrary bytes — quotes, newlines, non-UTF-8 — survive the
// shell round-trip untouched, rather than being mangled by quoting or word
// splitting. Piping avoids relying on the sandbox providing stdin.
func putFile(ctx context.Context, ec *agent.ExecCtx, path, content string) (agent.ExecResult, error) {
	q := shellQuote(path)
	b64 := shellQuote(base64.StdEncoding.EncodeToString([]byte(content)))
	script := fmt.Sprintf(`mkdir -p -- "$(dirname -- %s)" && printf %%s %s | base64 -d > %s`, q, b64, q)
	return runBash(ctx, ec, script)
}

// execErr renders the most useful message from a failed command: stderr if the
// command wrote any, else stdout, else a caller-supplied fallback so the model
// always gets something actionable back.
func execErr(res agent.ExecResult, fallback string) string {
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return s
	}
	if s := strings.TrimSpace(res.Stdout); s != "" {
		return s
	}
	return fallback
}
