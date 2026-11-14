package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
)

func TestAnthropicProvider_AdaptiveThinkingAndMultiToolBatch(t *testing.T) {
	var requests []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-anthropic-key" {
			t.Fatalf("unexpected Authorization: %q", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		requests = append(requests, body)

		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{
				"id": "msg_1",
				"type": "message",
				"role": "assistant",
				"model": "claude-sonnet-5-5",
				"stop_reason": "tool_use",
				"stop_sequence": "",
				"stop_details": null,
				"content": [
					{"type": "thinking", "thinking": "Inspecting auth.go and db.go", "signature": "sig_123"},
					{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": {"path": "auth.go"}},
					{"type": "tool_use", "id": "toolu_2", "name": "read_file", "input": {"path": "db.go"}}
				],
				"usage": {
					"input_tokens": 100,
					"cache_creation_input_tokens": 500,
					"cache_read_input_tokens": 200,
					"output_tokens": 45,
					"cache_creation": {"ephemeral_5m_input_tokens": 500, "ephemeral_1h_input_tokens": 0},
					"inference_geo": "us",
					"output_tokens_details": {"thinking_tokens": 20},
					"server_tool_use": {"web_search_requests": 0, "web_fetch_requests": 0},
					"service_tier": "standard",
					"speed": "standard"
				}
			}`))
			return
		}

		_, _ = w.Write([]byte(`{
			"id": "msg_2",
			"type": "message",
			"role": "assistant",
			"model": "claude-sonnet-5-5",
			"stop_reason": "end_turn",
			"stop_sequence": "",
			"stop_details": null,
			"content": [
				{"type": "text", "text": "{\"ok\":true}"}
			],
			"usage": {
				"input_tokens": 50,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens": 700,
				"output_tokens": 15,
				"cache_creation": {"ephemeral_5m_input_tokens": 0, "ephemeral_1h_input_tokens": 0},
				"inference_geo": "us",
				"output_tokens_details": {"thinking_tokens": 0},
				"server_tool_use": {"web_search_requests": 0, "web_fetch_requests": 0},
				"service_tier": "standard",
				"speed": "standard"
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewProvider(WithAPIKey("test-anthropic-key"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if p.Name() != "anthropic" {
		t.Fatalf("expected anthropic, got %q", p.Name())
	}

	readSchema, _ := jsonschema.For[struct {
		Path string `json:"path"`
	}](nil)
	temp := 0.3

	resp1, err := p.Generate(context.Background(), llm.ChatRequest{
		Config: llm.ModelConfig{
			Provider:        "anthropic",
			Model:           "claude-sonnet-5-5",
			Temperature:     &temp,
			ReasoningEffort: "high",
		},
		SystemPrompt: "Review security.",
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "Start review"}},
		Tools: []llm.ToolDefinition{
			{Name: "read_file", Description: "Read file", Parameters: readSchema},
		},
		ToolChoice: "auto",
	})
	if err != nil {
		t.Fatalf("Generate turn 1: %v", err)
	}
	if resp1.FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls, got %q", resp1.FinishReason)
	}
	if resp1.Thinking != "Inspecting auth.go and db.go" || resp1.Message.ThinkingSignature != "sig_123" {
		t.Fatalf("unexpected thinking / signature: %q / %q", resp1.Thinking, resp1.Message.ThinkingSignature)
	}
	if len(resp1.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(resp1.ToolCalls))
	}
	if resp1.Usage.InputTokens != 800 || resp1.Usage.OutputTokens != 45 || resp1.Usage.CachedTokens != 200 {
		t.Fatalf("unexpected usage: %+v", resp1.Usage)
	}

	// Verify adaptive thinking & omitted temperature when thinking enabled
	if _, hasTemp := requests[0]["temperature"]; hasTemp {
		t.Fatalf("temperature should be omitted when adaptive thinking is enabled")
	}
	thinkingCfg, _ := requests[0]["thinking"].(map[string]any)
	if thinkingCfg["type"] != "adaptive" {
		t.Fatalf("expected adaptive thinking, got %v", thinkingCfg)
	}
	outputCfg, _ := requests[0]["output_config"].(map[string]any)
	if outputCfg["effort"] != "high" {
		t.Fatalf("expected output_config.effort=high, got %v", outputCfg)
	}

	// Turn 2: ensure consecutive RoleTool messages merge into one user message & thinking block is echoed
	outSchema, _ := jsonschema.For[struct {
		OK bool `json:"ok"`
	}](nil)
	resp2, err := p.Generate(context.Background(), llm.ChatRequest{
		Config: llm.ModelConfig{
			Provider: "anthropic",
			Model:    "claude-sonnet-5-5",
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Start review"},
			resp1.Message,
			{Role: llm.RoleTool, ToolCallID: "toolu_1", ToolName: "read_file", Content: "auth code"},
			{Role: llm.RoleTool, ToolCallID: "toolu_2", ToolName: "read_file", Content: "db code"},
		},
		ResponseSchema: outSchema,
	})
	if err != nil {
		t.Fatalf("Generate turn 2: %v", err)
	}
	if resp2.FinishReason != "stop" || resp2.Message.Content != `{"ok":true}` {
		t.Fatalf("unexpected resp2: %+v", resp2)
	}

	msgs, _ := requests[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 anthropic messages (user, assistant, merged tool_result user), got %d", len(msgs))
	}
	assistantMsg := msgs[1].(map[string]any)
	assistantBlocks := assistantMsg["content"].([]any)
	firstBlock := assistantBlocks[0].(map[string]any)
	if firstBlock["type"] != "thinking" || firstBlock["signature"] != "sig_123" {
		t.Fatalf("expected thinking block first in assistant message, got %v", firstBlock)
	}
	toolUserMsg := msgs[2].(map[string]any)
	toolBlocks := toolUserMsg["content"].([]any)
	if len(toolBlocks) != 2 {
		t.Fatalf("expected 2 merged tool_result blocks, got %d", len(toolBlocks))
	}
}
