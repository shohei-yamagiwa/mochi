package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"google.golang.org/genai"
)

const providerName = "gemini"

// Option configures a Google Gemini LLM provider.
type Option func(*options)

type options struct {
	apiKey     string
	baseURL    string
	backend    genai.Backend
	httpClient *http.Client
}

// WithAPIKey sets the Gemini API key (falls back to GEMINI_API_KEY / GOOGLE_API_KEY).
func WithAPIKey(apiKey string) Option {
	return func(o *options) {
		o.apiKey = apiKey
	}
}

// WithBaseURL overrides the Gemini API base URL (e.g., for testing).
func WithBaseURL(baseURL string) Option {
	return func(o *options) {
		o.baseURL = strings.TrimSpace(baseURL)
	}
}

// WithBackend selects the GenAI backend (defaults to BackendGeminiAPI).
func WithBackend(backend genai.Backend) Option {
	return func(o *options) {
		o.backend = backend
	}
}

// WithHTTPClient sets a custom *http.Client for API requests.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *options) {
		o.httpClient = hc
	}
}

// Provider implements llm.Provider using Google's official google.golang.org/genai SDK.
type Provider struct {
	opts options
}

// NewProvider creates a Gemini provider ("gemini").
func NewProvider(opts ...Option) (*Provider, error) {
	var cfg options
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.apiKey == "" {
		if v := os.Getenv("GEMINI_API_KEY"); v != "" {
			cfg.apiKey = v
		} else {
			cfg.apiKey = os.Getenv("GOOGLE_API_KEY")
		}
	}
	if cfg.backend == genai.BackendUnspecified {
		cfg.backend = genai.BackendGeminiAPI
	}
	return &Provider{opts: cfg}, nil
}

// Name returns "gemini".
func (p *Provider) Name() string {
	return providerName
}

func (p *Provider) buildClient(ctx context.Context, baseURL string) (*genai.Client, error) {
	effectiveURL := strings.TrimSpace(baseURL)
	if effectiveURL == "" {
		effectiveURL = p.opts.baseURL
	}
	clientCfg := &genai.ClientConfig{
		APIKey:     p.opts.apiKey,
		Backend:    p.opts.backend,
		HTTPClient: p.opts.httpClient,
	}
	if effectiveURL != "" {
		clientCfg.HTTPOptions = genai.HTTPOptions{BaseURL: effectiveURL}
	}
	client, err := genai.NewClient(ctx, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("gemini new client: %w", err)
	}
	return client, nil
}

// Generate sends a normalized ChatRequest via Models.GenerateContent.
func (p *Provider) Generate(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if strings.TrimSpace(req.Config.Model) == "" {
		return nil, fmt.Errorf("gemini: model is required")
	}

	client, err := p.buildClient(ctx, req.Config.BaseURL)
	if err != nil {
		return nil, err
	}

	contents, extraSystem, err := buildContents(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("gemini: build contents: %w", err)
	}

	cfg := &genai.GenerateContentConfig{}
	fullSystem := joinSystem(req.SystemPrompt, extraSystem)
	if fullSystem != "" {
		cfg.SystemInstruction = genai.NewContentFromText(fullSystem, genai.RoleUser)
	}
	if req.Config.Temperature != nil {
		cfg.Temperature = genai.Ptr(float32(*req.Config.Temperature))
	}
	if req.Config.MaxOutputTokens > 0 {
		cfg.MaxOutputTokens = int32(req.Config.MaxOutputTokens)
	}
	if level := mapThinkingLevel(req.Config.ReasoningEffort); level != "" {
		cfg.ThinkingConfig = &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingLevel:   level,
		}
	}

	if len(req.Tools) > 0 {
		decls := make([]*genai.FunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			schemaMap, err := llm.SchemaToMap(t.Parameters)
			if err != nil {
				return nil, fmt.Errorf("gemini: tool %q schema: %w", t.Name, err)
			}
			decls = append(decls, &genai.FunctionDeclaration{
				Name:                 t.Name,
				Description:          t.Description,
				ParametersJsonSchema: schemaMap,
			})
		}
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	if req.ToolChoice != "" {
		cfg.ToolConfig = mapToolConfig(req.ToolChoice)
	}

	if req.ResponseSchema != nil {
		schemaMap, err := llm.SchemaToMap(req.ResponseSchema)
		if err != nil {
			return nil, fmt.Errorf("gemini: response_schema: %w", err)
		}
		cfg.ResponseMIMEType = "application/json"
		cfg.ResponseJsonSchema = schemaMap
	}

	resp, err := client.Models.GenerateContent(ctx, req.Config.Model, contents, cfg)
	if err != nil {
		return nil, fmt.Errorf("gemini generate_content: %w", err)
	}
	return parseResponse(resp), nil
}

func joinSystem(base string, extra []string) string {
	var parts []string
	if strings.TrimSpace(base) != "" {
		parts = append(parts, base)
	}
	for _, e := range extra {
		if strings.TrimSpace(e) != "" {
			parts = append(parts, e)
		}
	}
	return strings.Join(parts, "\n\n")
}

func buildContents(messages []llm.Message) ([]*genai.Content, []string, error) {
	var contents []*genai.Content
	var extraSystem []string
	var pendingToolParts []*genai.Part

	flushToolParts := func() {
		if len(pendingToolParts) == 0 {
			return
		}
		contents = append(contents, genai.NewContentFromParts(pendingToolParts, genai.RoleUser))
		pendingToolParts = nil
	}

	for _, msg := range messages {
		if msg.Role == llm.RoleTool {
			respMap := parseToolResultMap(msg.Content)
			p := genai.NewPartFromFunctionResponse(msg.ToolName, respMap)
			if msg.ToolCallID != "" && p.FunctionResponse != nil {
				p.FunctionResponse.ID = msg.ToolCallID
			}
			pendingToolParts = append(pendingToolParts, p)
			continue
		}
		flushToolParts()

		switch msg.Role {
		case llm.RoleSystem:
			if msg.Content != "" {
				extraSystem = append(extraSystem, msg.Content)
			}
		case llm.RoleUser:
			contents = append(contents, genai.NewContentFromText(msg.Content, genai.RoleUser))
		case llm.RoleAssistant:
			var parts []*genai.Part
			if msg.Content != "" {
				parts = append(parts, genai.NewPartFromText(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				argsMap := map[string]any{}
				if len(tc.Arguments) > 0 {
					_ = json.Unmarshal(tc.Arguments, &argsMap)
				}
				p := genai.NewPartFromFunctionCall(tc.Name, argsMap)
				if p.FunctionCall != nil && tc.ID != "" {
					p.FunctionCall.ID = tc.ID
				}
				if len(tc.ThoughtSignature) > 0 {
					p.ThoughtSignature = tc.ThoughtSignature
				}
				parts = append(parts, p)
			}
			if len(parts) == 0 {
				parts = append(parts, genai.NewPartFromText(""))
			}
			contents = append(contents, genai.NewContentFromParts(parts, genai.RoleModel))
		default:
			return nil, nil, fmt.Errorf("unsupported message role %q", msg.Role)
		}
	}
	flushToolParts()
	return contents, extraSystem, nil
}

func parseToolResultMap(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil && m != nil {
		return m
	}
	return map[string]any{"output": raw}
}

func mapThinkingLevel(effort string) genai.ThinkingLevel {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return genai.ThinkingLevelMinimal
	case "low":
		return genai.ThinkingLevelLow
	case "medium":
		return genai.ThinkingLevelMedium
	case "high":
		return genai.ThinkingLevelHigh
	case "xhigh", "max":
		slog.Warn("gemini: reasoning_effort level clamped to ThinkingLevelHigh", "requested", effort, "applied", "high")
		return genai.ThinkingLevelHigh
	default:
		return ""
	}
}

func mapToolConfig(choice string) *genai.ToolConfig {
	cfg := &genai.FunctionCallingConfig{}
	switch choice {
	case "auto":
		cfg.Mode = genai.FunctionCallingConfigModeAuto
	case "required":
		cfg.Mode = genai.FunctionCallingConfigModeAny
	case "none":
		cfg.Mode = genai.FunctionCallingConfigModeNone
	default:
		cfg.Mode = genai.FunctionCallingConfigModeAny
		cfg.AllowedFunctionNames = []string{choice}
	}
	return &genai.ToolConfig{FunctionCallingConfig: cfg}
}

func parseResponse(resp *genai.GenerateContentResponse) *llm.ChatResponse {
	var textParts []string
	var thinkingParts []string
	var toolCalls []llm.ToolCall
	var finishReason string

	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		if cand.FinishReason != "" {
			finishReason = strings.ToLower(string(cand.FinishReason))
		}
		if cand.Content != nil {
			for i, part := range cand.Content.Parts {
				if part == nil {
					continue
				}
				if part.Thought && part.Text != "" {
					thinkingParts = append(thinkingParts, part.Text)
				} else if part.Text != "" {
					textParts = append(textParts, part.Text)
				}
				if part.FunctionCall != nil {
					argsBytes, err := json.Marshal(part.FunctionCall.Args)
					if err != nil || string(argsBytes) == "null" {
						argsBytes = []byte("{}")
					}
					callID := part.FunctionCall.ID
					if callID == "" {
						callID = fmt.Sprintf("gemini_call_%d", i)
					}
					toolCalls = append(toolCalls, llm.ToolCall{
						ID:               callID,
						Name:             part.FunctionCall.Name,
						Arguments:        argsBytes,
						ThoughtSignature: part.ThoughtSignature,
					})
				}
			}
		}
	}

	content := strings.Join(textParts, "\n")
	thinking := strings.Join(thinkingParts, "\n")

	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if finishReason == "" || finishReason == "stop" {
		finishReason = "stop"
	}

	var usage llm.TokenUsage
	if resp.UsageMetadata != nil {
		usage = llm.TokenUsage{
			InputTokens:  int(resp.UsageMetadata.PromptTokenCount),
			OutputTokens: int(resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount),
			CachedTokens: int(resp.UsageMetadata.CachedContentTokenCount),
		}
	}

	return &llm.ChatResponse{
		Message: llm.Message{
			Role:      llm.RoleAssistant,
			Content:   content,
			Thinking:  thinking,
			ToolCalls: toolCalls,
		},
		Thinking:     thinking,
		ToolCalls:    toolCalls,
		Usage:        usage,
		FinishReason: finishReason,
	}
}
