package openai

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

func TestOpenAIResponsesProvider_GenerateAndToolContinuity(t *testing.T) {
	var capturedBodies []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-openai-key" {
			t.Fatalf("unexpected Authorization header: %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var reqJSON map[string]any
		if err := json.Unmarshal(body, &reqJSON); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		capturedBodies = append(capturedBodies, reqJSON)

		w.Header().Set("Content-Type", "application/json")
		if len(capturedBodies) == 1 {
			_, _ = w.Write([]byte(`{
				"id": "resp_1",
				"object": "response",
				"status": "completed",
				"output": [
					{
						"id": "rs_1",
						"type": "reasoning",
						"status": "completed",
						"summary": [{"type": "summary_text", "text": "Need to inspect main.go"}]
					},
					{
						"id": "fc_1",
						"type": "function_call",
						"call_id": "call_abc",
						"name": "read_file",
						"arguments": "{\"path\":\"main.go\",\"start_line\":1,\"end_line\":20}",
						"status": "completed"
					}
				],
				"usage": {
					"input_tokens": 120,
					"input_tokens_details": {"cached_tokens": 40, "cache_write_tokens": 0},
					"output_tokens": 35,
					"output_tokens_details": {"reasoning_tokens": 15},
					"total_tokens": 155
				}
			}`))
			return
		}

		_, _ = w.Write([]byte(`{
			"id": "resp_2",
			"object": "response",
			"status": "completed",
			"output": [
				{
					"id": "msg_1",
					"type": "message",
					"role": "assistant",
					"status": "completed",
					"content": [{"type": "output_text", "text": "{\"summary\":\"all good\"}"}]
				}
			],
			"usage": {
				"input_tokens": 200,
				"input_tokens_details": {"cached_tokens": 100, "cache_write_tokens": 0},
				"output_tokens": 25,
				"output_tokens_details": {"reasoning_tokens": 0},
				"total_tokens": 225
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewProvider(WithAPIKey("test-openai-key"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if p.Name() != "openai" {
		t.Fatalf("expected name openai, got %q", p.Name())
	}

	toolSchema, err := jsonschema.For[struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}](nil)
	if err != nil {
		t.Fatalf("jsonschema.For: %v", err)
	}

	temp := 0.2
	resp1, err := p.Generate(context.Background(), llm.ChatRequest{
		Config: llm.ModelConfig{
			Provider:        "openai",
			Model:           "gpt-6.1-sol",
			Temperature:     &temp,
			MaxOutputTokens: 1024,
			ReasoningEffort: "high",
		},
		SystemPrompt: "You are a reviewer.",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Review main.go"},
		},
		Tools: []llm.ToolDefinition{
			{Name: "read_file", Description: "Read a file", Parameters: toolSchema},
		},
		ToolChoice: "read_file",
	})
	if err != nil {
		t.Fatalf("Generate turn 1: %v", err)
	}
	if resp1.FinishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", resp1.FinishReason)
	}
	if resp1.Thinking != "Need to inspect main.go" {
		t.Fatalf("unexpected thinking: %q", resp1.Thinking)
	}
	if len(resp1.ToolCalls) != 1 || resp1.ToolCalls[0].ID != "call_abc" || resp1.ToolCalls[0].Name != "read_file" {
		t.Fatalf("unexpected tool calls: %+v", resp1.ToolCalls)
	}
	if resp1.Usage.InputTokens != 120 || resp1.Usage.OutputTokens != 35 || resp1.Usage.CachedTokens != 40 {
		t.Fatalf("unexpected usage: %+v", resp1.Usage)
	}

	// Verify turn 1 payload
	if capturedBodies[0]["instructions"] != "You are a reviewer." {
		t.Fatalf("missing instructions: %v", capturedBodies[0]["instructions"])
	}
	reasoningMap, _ := capturedBodies[0]["reasoning"].(map[string]any)
	if reasoningMap["effort"] != "high" {
		t.Fatalf("expected reasoning effort high, got %v", reasoningMap)
	}
	tcChoice, _ := capturedBodies[0]["tool_choice"].(map[string]any)
	if tcChoice["type"] != "function" || tcChoice["name"] != "read_file" {
		t.Fatalf("expected function tool choice, got %v", capturedBodies[0]["tool_choice"])
	}

	// Turn 2: supply assistant message (with ProviderItems) + tool output + response schema
	outSchema, err := jsonschema.For[struct {
		Summary string `json:"summary"`
	}](nil)
	if err != nil {
		t.Fatalf("jsonschema.For outSchema: %v", err)
	}
	resp2, err := p.Generate(context.Background(), llm.ChatRequest{
		Config: llm.ModelConfig{
			Provider: "openai",
			Model:    "gpt-6.1-sol",
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Review main.go"},
			resp1.Message,
			{Role: llm.RoleTool, ToolCallID: "call_abc", ToolName: "read_file", Content: "package main"},
		},
		ResponseSchema: outSchema,
	})
	if err != nil {
		t.Fatalf("Generate turn 2: %v", err)
	}
	if resp2.FinishReason != "stop" {
		t.Fatalf("expected finish_reason stop, got %q", resp2.FinishReason)
	}
	if resp2.Message.Content != `{"summary":"all good"}` {
		t.Fatalf("unexpected content: %q", resp2.Message.Content)
	}

	inputSlice, _ := capturedBodies[1]["input"].([]any)
	if len(inputSlice) != 4 {
		t.Fatalf("expected 4 input items (user + reasoning + function_call + function_call_output), got %d: %#v", len(inputSlice), inputSlice)
	}
	if textCfg, ok := capturedBodies[1]["text"].(map[string]any); !ok || textCfg["format"] == nil {
		t.Fatalf("expected structured text.format schema, got %v", capturedBodies[1]["text"])
	}
}

func TestOpenAICompatProvider_BaseURLAndTools(t *testing.T) {
	var capturedPath string
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl_1",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "qwen2.5-coder",
			"choices": [{
				"index": 0,
				"finish_reason": "tool_calls",
				"message": {
					"role": "assistant",
					"content": "",
					"refusal": "",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "list_dir",
							"arguments": "{\"path\":\".\"}"
						}
					}]
				}
			}],
			"usage": {
				"prompt_tokens": 55,
				"completion_tokens": 12,
				"total_tokens": 67,
				"prompt_tokens_details": {"cached_tokens": 10}
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewCompatProvider(WithAPIKey("local-key"))
	if err != nil {
		t.Fatalf("NewCompatProvider: %v", err)
	}
	if p.Name() != "openai_compat" {
		t.Fatalf("expected name openai_compat, got %q", p.Name())
	}

	toolSchema, _ := jsonschema.For[struct {
		Path string `json:"path"`
	}](nil)

	resp, err := p.Generate(context.Background(), llm.ChatRequest{
		Config: llm.ModelConfig{
			Provider:        "openai_compat",
			Model:           "qwen2.5-coder",
			BaseURL:         srv.URL,
			ReasoningEffort: "medium",
		},
		SystemPrompt: "System prompt",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "List files"},
		},
		Tools: []llm.ToolDefinition{
			{Name: "list_dir", Description: "List directory", Parameters: toolSchema},
		},
		ToolChoice: "required",
	})
	if err != nil {
		t.Fatalf("CompatProvider.Generate: %v", err)
	}
	if capturedPath != "/chat/completions" {
		t.Fatalf("unexpected path: %s", capturedPath)
	}
	if capturedBody["tool_choice"] != "required" {
		t.Fatalf("expected tool_choice=required, got %v", capturedBody["tool_choice"])
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "list_dir" {
		t.Fatalf("unexpected tool calls: %+v", resp.ToolCalls)
	}
	if resp.Usage.InputTokens != 55 || resp.Usage.OutputTokens != 12 || resp.Usage.CachedTokens != 10 {
		t.Fatalf("unexpected usage: %+v", resp.Usage)
	}
}

func TestOpenAI_ReasoningEffortMapping(t *testing.T) {
	var efforts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		effort := ""
		if rm, ok := body["reasoning"].(map[string]any); ok {
			effort, _ = rm["effort"].(string)
		}
		efforts = append(efforts, effort)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	p, err := NewProvider(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		in   string
		want string
	}{
		{"minimal", "minimal"},
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "max"},
		{"none", ""},
	}
	for _, tc := range cases {
		if _, err := p.Generate(context.Background(), llm.ChatRequest{
			Config:   llm.ModelConfig{Provider: "openai", Model: "gpt-6.1-sol", ReasoningEffort: tc.in},
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		}); err != nil {
			t.Fatalf("Generate(%q): %v", tc.in, err)
		}
	}
	for i, tc := range cases {
		if efforts[i] != tc.want {
			t.Fatalf("reasoning_effort %q mapped to %q, want %q", tc.in, efforts[i], tc.want)
		}
	}
}

