package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// TestAToolThatAsksForNothingCanBeCalledWithNothing covers what a model does
// with a tool that has no arguments: some send `{}`, some send nothing at all,
// some send a literal null. All three mean the same thing, and a tool whose
// whole input is "which session am I in" should run rather than come back
// complaining about the arguments it never wanted.
func TestAToolThatAsksForNothingCanBeCalledWithNothing(t *testing.T) {
	for _, input := range []string{"{}", "", "   ", "null"} {
		t.Run("input "+input, func(t *testing.T) {
			ran := false
			tool := AdaptFunc(ToolMeta{Name: "ping"},
				func(context.Context, *ExecCtx, struct{}) (ToolResult, error) {
					ran = true
					return ToolResult{Content: "pong"}, nil
				},
			)

			res, err := tool.Execute(context.Background(), &ExecCtx{}, json.RawMessage(input))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !ran {
				t.Fatalf("the tool did not run: %q", res.Content)
			}
			if res.IsError {
				t.Errorf("IsError = true (%q), want the call to have been accepted", res.Content)
			}
		})
	}
}

// TestArgumentsThatAreStillWrongAreStillRefused checks the leniency above is
// only about absence: a tool that needs an argument still tells the model when
// it is missing, which is what lets the next turn fix it.
func TestArgumentsThatAreStillWrongAreStillRefused(t *testing.T) {
	type args struct {
		Path string `json:"path"`
	}
	tool := AdaptFunc(ToolMeta{Name: "read"},
		func(context.Context, *ExecCtx, args) (ToolResult, error) {
			return ToolResult{Content: "read it"}, nil
		},
	)

	res, err := tool.Execute(context.Background(), &ExecCtx{}, json.RawMessage(`{"path": 7}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false (%q), want arguments of the wrong type to be refused", res.Content)
	}
}
