// Command mcp-inspect dials an MCP server and prints what it advertises: the
// negotiated protocol version, every tool the reflected door exposes with its
// required arguments, anything that was skipped, and the byte size each
// definition adds to a prompt.
//
// Reflection is the one part of an agent's tool set that a consumer cannot read
// in their own source — what reached the model came from a server, at runtime.
// This is what you run when it has gone wrong.
//
//	go run . https://example.com/mcp
//	go run . lightpanda mcp
//
// An argument starting with http:// or https:// is an endpoint; anything else is
// a command to run and speak to over stdio.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"

	"github.com/maikdotfi/metaharness/agent"
	"github.com/maikdotfi/metaharness/mcp"
	"github.com/maikdotfi/metaharness/model"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		log.Fatal("usage: mcp-inspect <endpoint> | mcp-inspect <command> [args...]")
	}
	if err := run(context.Background(), args); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, args []string) error {
	srv := open(args, mcp.WithObserver(report))
	defer srv.Close()

	tools, err := srv.Tools(ctx)
	if err != nil {
		return err
	}

	slices.SortFunc(tools, func(a, b agent.Tool) int {
		return strings.Compare(a.Definition().Name, b.Definition().Name)
	})

	total := 0
	for _, t := range tools {
		def := t.Definition()
		size := definitionSize(def)
		total += size
		fmt.Printf("\n%s  (%d bytes)\n", def.Name, size)
		if def.Description != "" {
			fmt.Printf("  %s\n", firstLine(def.Description))
		}
		if required := requiredArgs(def.Schema); len(required) > 0 {
			fmt.Printf("  requires: %s\n", strings.Join(required, ", "))
		}
	}

	fmt.Printf("\n%d tools, %d bytes of definitions\n", len(tools), total)
	return nil
}

// open picks the transport from the first argument, so there is no flag to get
// wrong: an endpoint looks like one, and everything else is a command.
func open(args []string, opts ...mcp.Option) *mcp.Server {
	if strings.HasPrefix(args[0], "http://") || strings.HasPrefix(args[0], "https://") {
		return mcp.HTTP(args[0], opts...)
	}
	return mcp.Stdio(args[0], args[1:], opts...)
}

func report(ev mcp.Event) {
	switch ev.Type {
	case mcp.EventDialed:
		if ev.Err != nil {
			fmt.Printf("could not dial %s: %v\n", ev.Server, ev.Err)
			return
		}
		fmt.Printf("%s speaks protocol %s\n", ev.Server, ev.Protocol)
	case mcp.EventSkipped:
		fmt.Printf("skipped %q: %v\n", ev.Tool, ev.Err)
	}
}

// definitionSize is what one tool costs the prompt. It is the column nobody
// measures until their system prompt has doubled.
func definitionSize(def model.ToolDefinition) int {
	encoded, err := json.Marshal(def)
	if err != nil {
		return 0
	}
	return len(encoded)
}

func requiredArgs(schema map[string]any) []string {
	raw, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, r := range raw {
		if name, ok := r.(string); ok {
			out = append(out, name)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
