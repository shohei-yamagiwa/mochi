package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
)

const (
	providerName           = "anthropic"
	defaultMaxOutputTokens = 8192
)

// Option configures an Anthropic LLM provider.
type Option func(*options)

type options struct {
	apiKey      string
	baseURL     string
	httpClient  *http.Client
	requestOpts []option.RequestOption
}

// WithAPIKey sets the Anthropic API key (falls back to ANTHROPIC_API_KEY env var).
func WithAPIKey(apiKey string) Option {
	return func(o *options) {
		o.apiKey = apiKey
	}
}

// WithBaseURL sets a custom API base URL.
func WithBaseURL(baseURL string) Option {
	return func(o *options) {
		o.baseURL = strings.TrimSpace(baseURL)
	}
}

// WithHTTPClient sets a custom *http.Client for API requests.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *options) {
		o.httpClient = hc
	}
}

// WithRequestOptions appends raw anthropic-sdk-go request options.
func WithRequestOptions(opts ...option.RequestOption) Option {
	return func(o *options) {
		o.requestOpts = append(o.requestOpts, opts...)
	}
}

// Provider implements llm.Provider using the official Anthropic Go SDK v2 (Messages API + Adaptive Thinking).
type Provider struct {
	opts   options
	client anthropicsdk.Client
}

// NewProvider creates an Anthropic Messages API provider ("anthropic").
func NewProvider(opts ...Option) (*Provider, error) {
	var cfg options
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.apiKey == "" {
		cfg.apiKey = os.Getenv("ANTHROPIC_API_KEY")
	}

	client := anthropicsdk.NewClient(buildRequestOptions(cfg, "")...)
	return &Provider{
		opts:   cfg,
		client: client,
	}, nil
}

func buildRequestOptions(cfg options, overrideBaseURL string) []option.RequestOption {
	var reqOpts []option.RequestOption
	if cfg.apiKey != "" {
		reqOpts = append(reqOpts, option.WithoutEnvironmentDefaults(), option.WithAPIKey(cfg.apiKey))
	}
	baseURL := strings.TrimSpace(overrideBaseURL)
	if baseURL == "" {
		baseURL = cfg.baseURL
	}
	if baseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(baseURL))
	}
	if cfg.httpClient != nil {
		reqOpts = append(reqOpts, option.WithHTTPClient(cfg.httpClient))
	}
	reqOpts = append(reqOpts, cfg.requestOpts...)
	return reqOpts
}

func (p *Provider) clientForRequest(baseURL string) anthropicsdk.Client {
	if strings.TrimSpace(baseURL) == "" {
		return p.client
	}
	return anthropicsdk.NewClient(buildRequestOptions(p.opts, baseURL)...)
}

// Name returns "anthropic".
func (p *Provider) Name() string {
	return providerName
}

// Generate sends a normalized ChatRequest via the Anthropic Messages API.
func (p *Provider) Generate(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if strings.TrimSpace(req.Config.Model) == "" {
		return nil, fmt.Errorf("anthropic: model is required")
	}

	maxTokens := int64(req.Config.MaxOutputTokens)
	if maxTokens <= 0 {
		maxTokens = defaultMaxOutputTokens
	}

	messages, extraSystem, err := buildMessages(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build messages: %w", err)
	}

	params := anthropicsdk.MessageNewParams{
		Model:     anthropicsdk.Model(req.Config.Model),
		MaxTokens: maxTokens,
		Messages:  messages,
	}

	var systemBlocks []anthropicsdk.TextBlockParam
	if req.SystemPrompt != "" {
		systemBlocks = append(systemBlocks, anthropicsdk.TextBlockParam{
			Text:         req.SystemPrompt,
			CacheControl: anthropicsdk.NewCacheControlEphemeralParam(),
		})
	}
	for _, s := range extraSystem {
		systemBlocks = append(systemBlocks, anthropicsdk.TextBlockParam{
			Text: s,
		})
	}
	if len(systemBlocks) > 0 {
		params.System = systemBlocks
	}

	if effort := mapEffort(req.Config.ReasoningEffort); effort != "" {
		params.Thinking = anthropicsdk.ThinkingConfigParamUnion{
			OfAdaptive: &anthropicsdk.ThinkingConfigAdaptiveParam{
				Display: anthropicsdk.ThinkingConfigAdaptiveDisplaySummarized,
			},
		}
		params.OutputConfig.Effort = effort
	}

	if len(req.Tools) > 0 {
		tools := make([]anthropicsdk.ToolUnionParam, 0, len(req.Tools))
		for i, t := range req.Tools {
			schemaMap, err := llm.SchemaToMap(t.Parameters)
			if err != nil {
				return nil, fmt.Errorf("anthropic: tool %q schema: %w", t.Name, err)
			}
			inputSchema := toToolInputSchema(schemaMap)
			tp := anthropicsdk.ToolParam{
				Name:        t.Name,
				InputSchema: inputSchema,
			}
			if t.Description != "" {
				tp.Description = anthropicsdk.String(t.Description)
			}
			if i == len(req.Tools)-1 {
				tp.CacheControl = anthropicsdk.NewCacheControlEphemeralParam()
			}
			tools = append(tools, anthropicsdk.ToolUnionParam{OfTool: &tp})
		}
		params.Tools = tools
	}

	if req.ToolChoice != "" {
		params.ToolChoice = mapToolChoice(req.ToolChoice)
	}

	if req.ResponseSchema != nil {
		schemaMap, err := llm.SchemaToMap(req.ResponseSchema)
		if err != nil {
			return nil, fmt.Errorf("anthropic: response_schema: %w", err)
		}
		params.OutputConfig.Format = anthropicsdk.JSONOutputFormatParam{
			Schema: schemaMap,
		}
	}

	client := p.clientForRequest(req.Config.BaseURL)
	resp, err := client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic messages.new: %w", err)
	}

	return parseMessageResponse(resp), nil
}

func buildMessages(messages []llm.Message) ([]anthropicsdk.MessageParam, []string, error) {
	var out []anthropicsdk.MessageParam
	var extraSystem []string
	var pendingToolResults []anthropicsdk.ContentBlockParamUnion

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		out = append(out, anthropicsdk.NewUserMessage(pendingToolResults...))
		pendingToolResults = nil
	}

	for _, msg := range messages {
		if msg.Role == llm.RoleTool {
			pendingToolResults = append(pendingToolResults, anthropicsdk.NewToolResultBlock(msg.ToolCallID, msg.Content, false))
			continue
		}
		flushToolResults()

		switch msg.Role {
		case llm.RoleSystem:
			if msg.Content != "" {
				extraSystem = append(extraSystem, msg.Content)
			}
		case llm.RoleUser:
			out = append(out, anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(msg.Content)))
		case llm.RoleAssistant:
			var blocks []anthropicsdk.ContentBlockParamUnion
			if msg.Thinking != "" && msg.ThinkingSignature != "" {
				blocks = append(blocks, anthropicsdk.NewThinkingBlock(msg.ThinkingSignature, msg.Thinking))
			}
			if msg.Content != "" {
				blocks = append(blocks, anthropicsdk.NewTextBlock(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				var input any = map[string]any{}
				if len(tc.Arguments) > 0 {
					var parsed any
					if err := json.Unmarshal(tc.Arguments, &parsed); err == nil && parsed != nil {
						input = parsed
					}
				}
				blocks = append(blocks, anthropicsdk.NewToolUseBlock(tc.ID, input, tc.Name))
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropicsdk.NewTextBlock(""))
			}
			out = append(out, anthropicsdk.NewAssistantMessage(blocks...))
		default:
			return nil, nil, fmt.Errorf("unsupported message role %q", msg.Role)
		}
	}
	flushToolResults()
	return out, extraSystem, nil
}

func toToolInputSchema(schemaMap map[string]any) anthropicsdk.ToolInputSchemaParam {
	s := anthropicsdk.ToolInputSchemaParam{}
	if props, ok := schemaMap["properties"]; ok {
		s.Properties = props
	}
	if reqRaw, ok := schemaMap["required"].([]any); ok {
		required := make([]string, 0, len(reqRaw))
		for _, r := range reqRaw {
			if str, ok := r.(string); ok {
				required = append(required, str)
			}
		}
		s.Required = required
	}
	extra := make(map[string]any)
	for k, v := range schemaMap {
		if k == "type" || k == "properties" || k == "required" {
			continue
		}
		extra[k] = v
	}
	if len(extra) > 0 {
		s.ExtraFields = extra
	}
	return s
}

func mapEffort(effort string) anthropicsdk.OutputConfigEffort {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return anthropicsdk.OutputConfigEffortLow
	case "medium":
		return anthropicsdk.OutputConfigEffortMedium
	case "high":
		return anthropicsdk.OutputConfigEffortHigh
	case "xhigh":
		return anthropicsdk.OutputConfigEffortXhigh
	case "max":
		return anthropicsdk.OutputConfigEffortMax
	default:
		return ""
	}
}

func mapToolChoice(choice string) anthropicsdk.ToolChoiceUnionParam {
	switch choice {
	case "auto":
		return anthropicsdk.ToolChoiceUnionParam{OfAuto: &anthropicsdk.ToolChoiceAutoParam{}}
	case "required":
		return anthropicsdk.ToolChoiceUnionParam{OfAny: &anthropicsdk.ToolChoiceAnyParam{}}
	case "none":
		return anthropicsdk.ToolChoiceUnionParam{OfNone: &anthropicsdk.ToolChoiceNoneParam{}}
	default:
		return anthropicsdk.ToolChoiceParamOfTool(choice)
	}
}

func parseMessageResponse(resp *anthropicsdk.Message) *llm.ChatResponse {
	var textParts []string
	var thinkingParts []string
	var thinkingSig string
	var toolCalls []llm.ToolCall

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			tb := block.AsText()
			if tb.Text != "" {
				textParts = append(textParts, tb.Text)
			}
		case "thinking":
			tb := block.AsThinking()
			if tb.Thinking != "" {
				thinkingParts = append(thinkingParts, tb.Thinking)
			}
			if tb.Signature != "" {
				thinkingSig = tb.Signature
			}
		case "tool_use":
			tu := block.AsToolUse()
			args := tu.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			toolCalls = append(toolCalls, llm.ToolCall{
				ID:        tu.ID,
				Name:      tu.Name,
				Arguments: args,
			})
		}
	}

	content := strings.Join(textParts, "\n")
	thinking := strings.Join(thinkingParts, "\n")

	finishReason := string(resp.StopReason)
	switch resp.StopReason {
	case anthropicsdk.StopReasonToolUse:
		finishReason = "tool_calls"
	case anthropicsdk.StopReasonEndTurn, anthropicsdk.StopReasonStopSequence:
		finishReason = "stop"
	case anthropicsdk.StopReasonMaxTokens:
		finishReason = "max_tokens"
	}

	totalInput := resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens

	return &llm.ChatResponse{
		Message: llm.Message{
			Role:              llm.RoleAssistant,
			Content:           content,
			Thinking:          thinking,
			ThinkingSignature: thinkingSig,
			ToolCalls:         toolCalls,
		},
		Thinking:  thinking,
		ToolCalls: toolCalls,
		Usage: llm.TokenUsage{
			InputTokens:  int(totalInput),
			OutputTokens: int(resp.Usage.OutputTokens),
			CachedTokens: int(resp.Usage.CacheReadInputTokens),
		},
		FinishReason: finishReason,
	}
}
