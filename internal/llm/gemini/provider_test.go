package gemini

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

func TestGeminiProvider_ThinkingToolSignatureAndStructuredOutput(t *testing.T) {
	var captured []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Goog-Api-Key"); got != "test-gemini-key" {
			t.Fatalf("unexpected X-Goog-Api-Key: %q", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var reqJSON map[string]any
		if err := json.Unmarshal(raw, &reqJSON); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		captured = append(captured, reqJSON)

		w.Header().Set("Content-Type", "application/json")
		if len(captured) == 1 {
			// Thought signature "c2lnX3Rlc3Q=" is base64("sig_test")
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"finishReason": "STOP",
					"content": {
						"role": "model",
						"parts": [
							{"thought": true, "text": "Checking diff lines"},
							{
								"functionCall": {
									"id": "call_g1",
									"name": "search_code",
									"args": {"pattern": "TODO"}
								},
								"thoughtSignature": "c2lnX3Rlc3Q="
							}
						]
					}
				}],
				"usageMetadata": {
					"promptTokenCount": 90,
					"candidatesTokenCount": 20,
					"thoughtsTokenCount": 12,
					"cachedContentTokenCount": 30,
					"totalTokenCount": 122
				}
			}`))
			return
		}

		_, _ = w.Write([]byte(`{
			"candidates": [{
				"finishReason": "STOP",
				"content": {
					"role": "model",
					"parts": [
						{"text": "{\"findings\":[]}"}
					]
				}
			}],
			"usageMetadata": {
				"promptTokenCount": 140,
				"candidatesTokenCount": 10,
				"totalTokenCount": 150
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewProvider(WithAPIKey("test-gemini-key"), WithBaseURL(srv.URL))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if p.Name() != "gemini" {
		t.Fatalf("expected gemini, got %q", p.Name())
	}

	searchSchema, _ := jsonschema.For[struct {
		Pattern string `json:"pattern"`
	}](nil)
	resp1, err := p.Generate(context.Background(), llm.ChatRequest{
		Config: llm.ModelConfig{
			Provider:        "gemini",
			Model:           "gemini-3.8-flash",
			ReasoningEffort: "high",
		},
		SystemPrompt: "Analyze performance.",
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: "Find hot paths"}},
		Tools: []llm.ToolDefinition{
			{Name: "search_code", Description: "Search code", Parameters: searchSchema},
		},
		ToolChoice: "search_code",
	})
	if err != nil {
		t.Fatalf("Generate turn 1: %v", err)
	}
	if resp1.FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls, got %q", resp1.FinishReason)
	}
	if resp1.Thinking != "Checking diff lines" {
		t.Fatalf("unexpected thinking: %q", resp1.Thinking)
	}
	if len(resp1.ToolCalls) != 1 || string(resp1.ToolCalls[0].ThoughtSignature) != "sig_test" {
		t.Fatalf("expected ThoughtSignature sig_test preserved, got %+v", resp1.ToolCalls)
	}
	if resp1.Usage.InputTokens != 90 || resp1.Usage.OutputTokens != 32 || resp1.Usage.CachedTokens != 30 {
		t.Fatalf("unexpected usage: %+v", resp1.Usage)
	}

	outSchema, _ := jsonschema.For[struct {
		Findings []string `json:"findings"`
	}](nil)
	resp2, err := p.Generate(context.Background(), llm.ChatRequest{
		Config: llm.ModelConfig{
			Provider: "gemini",
			Model:    "gemini-3.8-flash",
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Find hot paths"},
			resp1.Message,
			{Role: llm.RoleTool, ToolCallID: "call_g1", ToolName: "search_code", Content: "no matches"},
		},
		ResponseSchema: outSchema,
	})
	if err != nil {
		t.Fatalf("Generate turn 2: %v", err)
	}
	if resp2.FinishReason != "stop" || resp2.Message.Content != `{"findings":[]}` {
		t.Fatalf("unexpected resp2: %+v", resp2)
	}

	// Verify ThoughtSignature was echoed back on turn 2 model functionCall part
	contents := captured[1]["contents"].([]any)
	modelTurn := contents[1].(map[string]any)
	parts := modelTurn["parts"].([]any)
	fcPart := parts[0].(map[string]any)
	if fcPart["thoughtSignature"] != "c2lnX3Rlc3Q=" {
		t.Fatalf("expected echoed thoughtSignature c2lnX3Rlc3Q=, got %v", fcPart["thoughtSignature"])
	}
	genCfg := captured[1]["generationConfig"].(map[string]any)
	if genCfg["responseMimeType"] != "application/json" || genCfg["responseJsonSchema"] == nil {
		t.Fatalf("expected responseMimeType/responseJsonSchema, got %v", genCfg)
	}
}
