//go:build lightpanda

// These tests drive a real `lightpanda mcp` subprocess. Run them with
// `make test-lightpanda`; they need lightpanda on PATH.
//
// Pinned against lightpanda 1.0.0-nightly.5445, which negotiates 2024-11-05.
// That is deliberate: this is the one place the legacy lifecycle is proven,
// since every unit test in the package runs against a current-protocol SDK
// server.
package lightpanda_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/mcp"
	"github.com/maikdotfi/metaharness/mcp/lightpanda"
)

const pageHTML = `<!doctype html>
<html><body>
<h1>Bordeaux</h1>
<p>A city on the Garonne.</p>
<a href="https://example.com/wine">wine</a>
</body></html>`

// page serves fixed content, so what markdown returns is a fact rather than
// whatever the internet felt like today.
func page(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(pageHTML))
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// browserPIDs is how the tests see the subprocess they never hold a handle to.
func browserPIDs(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", "lightpanda mcp").Output()
	if err != nil && len(out) == 0 {
		return nil // pgrep exits non-zero when nothing matches
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids
}

// spawned is the pid that appeared since before, failing if that is not exactly
// one process.
func spawned(t *testing.T, before []int) int {
	t.Helper()
	var fresh []int
	for _, pid := range browserPIDs(t) {
		if !slices.Contains(before, pid) {
			fresh = append(fresh, pid)
		}
	}
	if len(fresh) != 1 {
		t.Fatalf("found %d new lightpanda processes, want 1", len(fresh))
	}
	return fresh[0]
}

func alive(pid int) bool {
	out, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pid=").Output()
	return strings.TrimSpace(string(out)) != ""
}

// TestTheReflectedDoorListsARealServersTools is one line of wiring against a
// server nobody wrote for us.
func TestTheReflectedDoorListsARealServersTools(t *testing.T) {
	var protocol string
	browser := mcp.Stdio("lightpanda", []string{"mcp"}, mcp.WithObserver(func(ev mcp.Event) {
		if ev.Type == mcp.EventDialed {
			protocol = ev.Protocol
		}
	}))
	defer browser.Close()

	tools, err := browser.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if protocol != "2024-11-05" {
		t.Errorf("protocol = %q, want 2024-11-05 — the pin may have moved", protocol)
	}

	var names []string
	for _, tool := range tools {
		names = append(names, tool.Definition().Name)
	}
	if !slices.Contains(names, "lightpanda_goto") {
		t.Errorf("tools = %v, want lightpanda_goto among them", names)
	}
}

// TestOneSubprocessKeepsThePageBetweenCalls is the interesting property of a
// stateful server: goto and markdown are separate calls, and the second reads
// what the first loaded. It works precisely because the connection is kept.
func TestOneSubprocessKeepsThePageBetweenCalls(t *testing.T) {
	browser := mcp.Stdio("lightpanda", []string{"mcp"})
	defer browser.Close()

	if _, err := browser.Call(context.Background(), "goto", map[string]any{"url": page(t)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	res, err := browser.Call(context.Background(), "markdown", map[string]any{})
	if err != nil {
		t.Fatalf("markdown: %v", err)
	}
	if !strings.Contains(res.Content, "Bordeaux") {
		t.Errorf("markdown = %q, want the page goto loaded", res.Content)
	}
}

// TestADeclaredToolAndAReflectedOneAgree pins that curation changed the names
// and the arguments, not the answer.
func TestADeclaredToolAndAReflectedOneAgree(t *testing.T) {
	url := page(t)
	ctx := context.Background()

	declared := mcp.Stdio("lightpanda", []string{"mcp"})
	defer declared.Close()
	tools := lightpanda.Tools(declared)
	if _, err := tool(t, tools, "browser_goto").Execute(ctx, &agent.ExecCtx{}, []byte(`{"url":"`+url+`"}`)); err != nil {
		t.Fatalf("browser_goto: %v", err)
	}
	viaDeclared, err := tool(t, tools, "browser_markdown").Execute(ctx, &agent.ExecCtx{}, []byte(`{}`))
	if err != nil {
		t.Fatalf("browser_markdown: %v", err)
	}

	reflected := mcp.Stdio("lightpanda", []string{"mcp"})
	defer reflected.Close()
	if _, err := reflected.Call(ctx, "goto", map[string]any{"url": url}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	viaReflected, err := reflected.Call(ctx, "markdown", map[string]any{})
	if err != nil {
		t.Fatalf("markdown: %v", err)
	}

	if viaDeclared.Content != viaReflected.Content {
		t.Errorf("declared = %q, reflected = %q, want the same page", viaDeclared.Content, viaReflected.Content)
	}
}

// TestCloseReapsTheChild is the one resource a Server owns, and the reason Close
// exists at all.
func TestCloseReapsTheChild(t *testing.T) {
	before := browserPIDs(t)
	browser := mcp.Stdio("lightpanda", []string{"mcp"})
	if _, err := browser.Tools(context.Background()); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	pid := spawned(t, before)

	if err := browser.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for range 50 {
		if !alive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("process %d is still running 5s after Close", pid)
}

// TestAKilledChildIsRedialedAndSaidSo is decision 7 against the server it was
// written for: nothing of ours was lost, but the page was, and the model has to
// be told rather than reading a blank one as fact.
func TestAKilledChildIsRedialedAndSaidSo(t *testing.T) {
	var redials int
	before := browserPIDs(t)
	browser := mcp.Stdio("lightpanda", []string{"mcp"}, mcp.WithObserver(func(ev mcp.Event) {
		if ev.Type == mcp.EventRedialed {
			redials++
		}
	}))
	defer browser.Close()

	ctx := context.Background()
	if _, err := browser.Call(ctx, "goto", map[string]any{"url": page(t)}); err != nil {
		t.Fatalf("goto: %v", err)
	}
	pid := spawned(t, before)
	if err := exec.Command("kill", "-9", strconv.Itoa(pid)).Run(); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// Whether the retried call succeeds or fails, the agent loop shows the model
	// one string, so that string is what the test reads. On lightpanda it fails:
	// the page really is gone, which is the whole point of saying so.
	res, err := browser.Call(ctx, "markdown", map[string]any{})
	seen := res.Content
	if err != nil {
		seen = err.Error()
	}

	if redials != 1 {
		t.Errorf("redials = %d, want 1", redials)
	}
	if !strings.Contains(seen, "reopened") {
		t.Errorf("markdown said %q, want it to say the connection was reopened", seen)
	}
}
