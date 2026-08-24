package agent

import (
	"context"
	"slices"
	"testing"
)

func stub(name string) Tool {
	return AdaptFunc(ToolMeta{Name: name},
		func(context.Context, *ExecCtx, struct{}) (ToolResult, error) {
			return ToolResult{Content: name}, nil
		},
	)
}

// TestToolsFromTwoPlacesAllArrive covers the wiring where built-in tools and
// tools discovered elsewhere — an MCP server, say — are passed separately. Both
// callers said "with these tools", and neither meant "instead of the others".
func TestToolsFromTwoPlacesAllArrive(t *testing.T) {
	a := New("prompt",
		WithTools(stub("bash")),
		WithTools(stub("browser_goto"), stub("browser_markdown")),
	)

	var names []string
	for name := range a.Tools {
		names = append(names, name)
	}
	slices.Sort(names)

	want := []string{"bash", "browser_goto", "browser_markdown"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

// TestTwoToolsWithOneNameIsAPanic pins that a name collision stops the program
// at wiring time rather than letting one tool silently shadow the other: the
// model would be told about one tool and reach the wrong implementation.
func TestTwoToolsWithOneNameIsAPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New did not panic on two tools named bash")
		}
	}()
	New("prompt", WithTools(stub("bash")), WithTools(stub("bash")))
}
