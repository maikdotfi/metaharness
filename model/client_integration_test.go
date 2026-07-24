//go:build integration

package model_test

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/maikdotfi/metaharness/model"
)

const defaultTestModel = "gemma4:31b-cloud"

type configuredModel struct {
	client model.ModelClient
	id     string
}

// newTestModel builds the public metaharness model from the environment.
// These tests are live provider smoke tests and skip when no API key is set.
func newTestModel(t *testing.T) configuredModel {
	t.Helper()

	baseURL := cmp.Or(os.Getenv("LLM_BASE_URL"), os.Getenv("ANTHROPIC_API_URL"))
	baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1/messages")
	apiKey := cmp.Or(os.Getenv("LLM_API_KEY"), os.Getenv("ANTHROPIC_API_KEY"))
	if apiKey == "" {
		t.Skip("set LLM_API_KEY or ANTHROPIC_API_KEY to run live model tests")
	}

	cfg := model.Config{
		Provider: model.Provider(cmp.Or(os.Getenv("LLM_PROVIDER"), string(model.ProviderAnthropic))),
		APIKey:   apiKey,
		BaseURL:  baseURL,
	}
	client, err := model.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return configuredModel{
		client: client,
		id:     cmp.Or(os.Getenv("LLM_MODEL"), defaultTestModel),
	}
}

func dump(t *testing.T, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Logf("cannot marshal value: %v", err)
		return
	}
	t.Log(string(data))
}

func messageText(message *model.Message) string {
	var text strings.Builder
	for _, part := range model.TextParts(message) {
		text.WriteString(part.Text)
	}
	return text.String()
}

func TestGenerateWithoutSession(t *testing.T) {
	configured := newTestModel(t)
	request := model.ModelRequest{
		Model: configured.id,
		Messages: []model.Message{
			model.NewUserMessage("write me a short poem about rainbows and Kubernetes"),
		},
	}
	dump(t, request)

	response, usage, err := configured.client.Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	dump(t, response)
	dump(t, usage)
	t.Log(messageText(&response))
}

func TestGenerateWithSession(t *testing.T) {
	configured := newTestModel(t)
	ctx := context.Background()
	session := []model.Message{
		model.NewUserMessage("write me a short poem about rainbows and Kubernetes"),
	}

	firstRequest := model.ModelRequest{Model: configured.id, Messages: session}
	dump(t, firstRequest)
	first, _, err := configured.client.Generate(ctx, firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	dump(t, first)
	t.Logf("first response:\n%s", messageText(&first))

	session = append(session, first)
	session = append(session, model.NewUserMessage("nice, now make it a rap instead but keep the same core wording"))
	secondRequest := model.ModelRequest{Model: configured.id, Messages: session}
	dump(t, secondRequest)
	second, _, err := configured.client.Generate(ctx, secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	dump(t, second)
	t.Logf("second response:\n%s", messageText(&second))
}

func TestGenerateWithSystemPrompt(t *testing.T) {
	configured := newTestModel(t)
	request := model.ModelRequest{
		Model:  configured.id,
		System: "You are a pirate. Always speak like a pirate, in every response, no matter what. Please answer concisely.",
		Messages: []model.Message{
			model.NewUserMessage("how would you debug a k8s Pod in CrashLoopBackOff?"),
		},
	}
	dump(t, request)

	response, _, err := configured.client.Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	dump(t, response)
	t.Log(messageText(&response))
}

func TestGenerateWithTool(t *testing.T) {
	configured := newTestModel(t)
	ctx := context.Background()
	tool := currentTimeTool()
	session := []model.Message{
		model.NewUserMessage("What is the current time? Use the current_time tool to find out."),
	}

	response, _, err := configured.client.Generate(ctx, model.ModelRequest{
		Model:    configured.id,
		Messages: session,
		Tools:    []model.ToolDefinition{tool.definition},
	})
	if err != nil {
		t.Fatal(err)
	}
	dump(t, response)

	calls := model.ToolCalls(&response)
	if len(calls) == 0 {
		t.Fatalf("expected the model to call a tool, got text instead: %q", messageText(&response))
	}

	session = append(session, response)
	results := runToolCalls(t, tool, calls)
	session = append(session, model.NewToolResultMessage(results...))
	dump(t, session[len(session)-1])

	final, _, err := configured.client.Generate(ctx, model.ModelRequest{
		Model:    configured.id,
		Messages: session,
		Tools:    []model.ToolDefinition{tool.definition},
	})
	if err != nil {
		t.Fatal(err)
	}
	dump(t, final)
	t.Logf("final answer:\n%s", messageText(&final))
}

func TestGenerateWithToolLoop(t *testing.T) {
	configured := newTestModel(t)
	ctx := context.Background()
	tool := currentTimeTool()
	session := []model.Message{
		model.NewUserMessage("What is the current time? Use the current_time tool to find out."),
	}

	const maxTurns = 10
	var final model.Message
	for turn := 1; ; turn++ {
		if turn > maxTurns {
			t.Fatalf("model did not finish within %d turns", maxTurns)
		}

		response, _, err := configured.client.Generate(ctx, model.ModelRequest{
			Model:    configured.id,
			Messages: session,
			Tools:    []model.ToolDefinition{tool.definition},
		})
		if err != nil {
			t.Fatal(err)
		}
		dump(t, response)

		calls := model.ToolCalls(&response)
		if len(calls) == 0 {
			final = response
			break
		}

		session = append(session, response)
		results := runToolCalls(t, tool, calls)
		session = append(session, model.NewToolResultMessage(results...))
		dump(t, session[len(session)-1])
	}

	t.Logf("final answer:\n%s", messageText(&final))
}

type testTool struct {
	definition model.ToolDefinition
	handler    func(string) (string, error)
}

func currentTimeTool() testTool {
	return testTool{
		definition: model.ToolDefinition{
			Name:        "current_time",
			Description: "Returns the current time with one second precision, formatted as an RFC3339 timestamp.",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		handler: func(string) (string, error) {
			return time.Now().Truncate(time.Second).Format(time.RFC3339), nil
		},
	}
}

func runToolCalls(t *testing.T, tool testTool, calls []model.ToolCall) []model.ToolResult {
	t.Helper()
	results := make([]model.ToolResult, 0, len(calls))
	for _, call := range calls {
		t.Logf("model called tool %q with input %s", call.Name, call.Input)
		if call.Name != tool.definition.Name {
			t.Fatalf("unexpected tool call: %q", call.Name)
		}
		output, err := tool.handler(call.Input)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, model.ToolResult{ID: call.ID, Text: output})
	}
	return results
}
