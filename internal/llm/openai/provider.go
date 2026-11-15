package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
)

const (
	providerNameOpenAI = "openai"
	providerNameCompat = "openai_compat"
)

// Option configures an OpenAI or OpenAI-compatible LLM provider.
type Option func(*options)

type options struct {
	apiKey      string
	baseURL     string
	httpClient  *http.Client
	requestOpts []option.RequestOption
}

// WithAPIKey sets the API key explicitly (falls back to OPENAI_API_KEY env var).
func WithAPIKey(apiKey string) Option {
	return func(o *options) {
		o.apiKey = apiKey
	}
}

// WithBaseURL sets a default base URL for API requests.
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

// WithRequestOptions appends raw openai-go request options.
func WithRequestOptions(opts ...option.RequestOption) Option {
	return func(o *options) {
		o.requestOpts = append(o.requestOpts, opts...)
	}
}

// Provider implements llm.Provider using the official OpenAI Go SDK (Responses API).
type Provider struct {
	name   string
	opts   options
	client openaisdk.Client
}

// NewProvider creates an OpenAI Responses API provider ("openai").
func NewProvider(opts ...Option) (*Provider, error) {
	return newProvider(providerNameOpenAI, opts...)
}

// NewCompatProvider creates an OpenAI Chat Completions provider ("openai_compat")
// suitable for Ollama, vLLM, OpenRouter, and other compatible endpoints.
func NewCompatProvider(opts ...Option) (*CompatProvider, error) {
	p, err := newProvider(providerNameCompat, opts...)
	if err != nil {
		return nil, err
	}
	return &CompatProvider{base: p}, nil
}

func newProvider(name string, opts ...Option) (*Provider, error) {
	var cfg options
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.apiKey == "" {
		if name == providerNameCompat {
			if v := os.Getenv("OPENAI_COMPAT_API_KEY"); v != "" {
				cfg.apiKey = v
			} else {
				cfg.apiKey = os.Getenv("OPENAI_API_KEY")
			}
		} else {
			cfg.apiKey = os.Getenv("OPENAI_API_KEY")
		}
	}

	reqOpts := buildRequestOptions(cfg, "")
	client := openaisdk.NewClient(reqOpts...)
	return &Provider{
		name:   name,
		opts:   cfg,
		client: client,
	}, nil
}

func buildRequestOptions(cfg options, overrideBaseURL string) []option.RequestOption {
	var reqOpts []option.RequestOption
	if cfg.apiKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(cfg.apiKey))
	} else {
		// openai-go requires non-empty auth header or skips if omitted; compat servers may ignore it,
		// while SDK constructor safely succeeds without APIKey if env is unset, or uses WithAPIKey when set.
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

func (p *Provider) clientForRequest(baseURL string) openaisdk.Client {
	if strings.TrimSpace(baseURL) == "" {
		return p.client
	}
	return openaisdk.NewClient(buildRequestOptions(p.opts, baseURL)...)
}

// Name returns "openai".
func (p *Provider) Name() string {
	return p.name
}

// Generate sends a normalized ChatRequest via the OpenAI Responses API.
func (p *Provider) Generate(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if strings.TrimSpace(req.Config.Model) == "" {
		return nil, fmt.Errorf("openai: model is required")
	}

	inputItems, err := buildResponsesInput(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("openai: build input items: %w", err)
	}

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(req.Config.Model),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: inputItems,
		},
	}
	if req.SystemPrompt != "" {
		params.Instructions = openaisdk.String(req.SystemPrompt)
	}
	if req.Config.MaxOutputTokens > 0 {
		params.MaxOutputTokens = openaisdk.Int(int64(req.Config.MaxOutputTokens))
	}
	if req.Config.Temperature != nil {
		params.Temperature = openaisdk.Float(*req.Config.Temperature)
	}
	if effort := mapReasoningEffort(req.Config.ReasoningEffort); effort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort:  effort,
			Summary: shared.ReasoningSummaryAuto,
		}
	}

	if len(req.Tools) > 0 {
		tools := make([]responses.ToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			schemaMap, err := llm.SchemaToMap(t.Parameters)
			if err != nil {
				return nil, fmt.Errorf("openai: tool %q schema: %w", t.Name, err)
			}
			ft := responses.ToolParamOfFunction(t.Name, schemaMap, false)
			if ft.OfFunction != nil && t.Description != "" {
				ft.OfFunction.Description = openaisdk.String(t.Description)
			}
			tools = append(tools, ft)
		}
		params.Tools = tools
	}

	if req.ToolChoice != "" {
		params.ToolChoice = mapResponsesToolChoice(req.ToolChoice)
	}

	if req.ResponseSchema != nil {
		schemaMap, err := llm.SchemaToMap(req.ResponseSchema)
		if err != nil {
			return nil, fmt.Errorf("openai: response_schema: %w", err)
		}
		params.Text = responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigParamOfJSONSchema("output_schema", schemaMap),
		}
	}

	client := p.clientForRequest(req.Config.BaseURL)
	resp, err := client.Responses.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openai responses.new: %w", err)
	}
	return parseResponsesOutput(resp), nil
}

func buildResponsesInput(messages []llm.Message) (responses.ResponseInputParam, error) {
	items := make(responses.ResponseInputParam, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleSystem:
			items = append(items, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleDeveloper))
		case llm.RoleUser:
			items = append(items, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleUser))
		case llm.RoleAssistant:
			if len(msg.ProviderItems) > 0 {
				restored := false
				for _, raw := range msg.ProviderItems {
					if item, ok := restoreResponseInputItem(raw); ok {
						items = append(items, item)
						restored = true
					}
				}
				if restored {
					if msg.Content != "" && !hasMessageItem(msg.ProviderItems) {
						items = append(items, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleAssistant))
					}
					continue
				}
			}
			if msg.Content != "" {
				items = append(items, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleAssistant))
			}
			for _, tc := range msg.ToolCalls {
				args := strings.TrimSpace(string(tc.Arguments))
				if args == "" {
					args = "{}"
				}
				items = append(items, responses.ResponseInputItemParamOfFunctionCall(args, tc.ID, tc.Name))
			}
		case llm.RoleTool:
			outItem := responses.ResponseInputItemParamOfFunctionCallOutput(msg.Content)
			if outItem.OfFunctionCallOutput != nil {
				outItem.OfFunctionCallOutput.CallID = openaisdk.String(msg.ToolCallID)
				if msg.ToolName != "" {
					outItem.OfFunctionCallOutput.Name = openaisdk.String(msg.ToolName)
				}
			}
			items = append(items, outItem)
		default:
			return nil, fmt.Errorf("unsupported message role %q", msg.Role)
		}
	}
	return items, nil
}

func restoreResponseInputItem(raw json.RawMessage) (responses.ResponseInputItemUnionParam, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return responses.ResponseInputItemUnionParam{}, false
	}
	switch probe.Type {
	case "reasoning":
		var r responses.ResponseReasoningItem
		if err := json.Unmarshal(raw, &r); err == nil {
			p := r.ToParam()
			return responses.ResponseInputItemUnionParam{OfReasoning: &p}, true
		}
	case "function_call":
		var fc responses.ResponseFunctionToolCall
		if err := json.Unmarshal(raw, &fc); err == nil {
			p := fc.ToParam()
			return responses.ResponseInputItemUnionParam{OfFunctionCall: &p}, true
		}
	case "message":
		var om responses.ResponseOutputMessage
		if err := json.Unmarshal(raw, &om); err == nil {
			p := om.ToParam()
			return responses.ResponseInputItemUnionParam{OfOutputMessage: &p}, true
		}
	}
	return responses.ResponseInputItemUnionParam{}, false
}

func hasMessageItem(rawItems []json.RawMessage) bool {
	for _, raw := range rawItems {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.Type == "message" {
			return true
		}
	}
	return false
}

func mapResponsesToolChoice(choice string) responses.ResponseNewParamsToolChoiceUnion {
	switch choice {
	case "auto":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openaisdk.Opt(responses.ToolChoiceOptionsAuto),
		}
	case "required":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openaisdk.Opt(responses.ToolChoiceOptionsRequired),
		}
	case "none":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openaisdk.Opt(responses.ToolChoiceOptionsNone),
		}
	default:
		return responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{Name: choice},
		}
	}
}

func mapReasoningEffort(effort string) shared.ReasoningEffort {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return shared.ReasoningEffortMinimal
	case "low":
		return shared.ReasoningEffortLow
	case "medium":
		return shared.ReasoningEffortMedium
	case "high":
		return shared.ReasoningEffortHigh
	case "xhigh":
		return shared.ReasoningEffortXhigh
	case "max":
		return shared.ReasoningEffortMax
	default:
		return ""
	}
}

func parseResponsesOutput(resp *responses.Response) *llm.ChatResponse {
	var textParts []string
	var thinkingParts []string
	var toolCalls []llm.ToolCall
	var providerItems []json.RawMessage

	for _, item := range resp.Output {
		if raw := strings.TrimSpace(item.RawJSON()); raw != "" {
			providerItems = append(providerItems, json.RawMessage(raw))
		}
		switch item.Type {
		case "message":
			msg := item.AsMessage()
			for _, content := range msg.Content {
				if content.Type == "output_text" {
					t := content.AsOutputText().Text
					if t != "" {
						textParts = append(textParts, t)
					}
				} else if content.Type == "refusal" {
					r := content.AsRefusal().Refusal
					if r != "" {
						textParts = append(textParts, r)
					}
				}
			}
		case "reasoning":
			r := item.AsReasoning()
			for _, s := range r.Summary {
				if s.Text != "" {
					thinkingParts = append(thinkingParts, s.Text)
				}
			}
			for _, c := range r.Content {
				if c.Text != "" {
					thinkingParts = append(thinkingParts, c.Text)
				}
			}
		case "function_call":
			fc := item.AsFunctionCall()
			args := json.RawMessage(strings.TrimSpace(fc.Arguments))
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			callID := fc.CallID
			if callID == "" {
				callID = fc.ID
			}
			toolCalls = append(toolCalls, llm.ToolCall{
				ID:        callID,
				Name:      fc.Name,
				Arguments: args,
			})
		}
	}

	content := strings.Join(textParts, "\n")
	if content == "" {
		content = resp.OutputText()
	}
	thinking := strings.Join(thinkingParts, "\n")

	finishReason := string(resp.Status)
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if finishReason == "completed" || finishReason == "" {
		finishReason = "stop"
	} else if finishReason == "incomplete" && resp.IncompleteDetails.Reason != "" {
		finishReason = resp.IncompleteDetails.Reason
	}

	assistantMsg := llm.Message{
		Role:          llm.RoleAssistant,
		Content:       content,
		Thinking:      thinking,
		ToolCalls:     toolCalls,
		ProviderItems: providerItems,
	}

	return &llm.ChatResponse{
		Message:   assistantMsg,
		Thinking:  thinking,
		ToolCalls: toolCalls,
		Usage: llm.TokenUsage{
			InputTokens:  int(resp.Usage.InputTokens),
			OutputTokens: int(resp.Usage.OutputTokens),
			CachedTokens: int(resp.Usage.InputTokensDetails.CachedTokens),
		},
		FinishReason: finishReason,
	}
}

// CompatProvider implements llm.Provider using the Chat Completions API ("openai_compat").
type CompatProvider struct {
	base *Provider
}

// Name returns "openai_compat".
func (c *CompatProvider) Name() string {
	return providerNameCompat
}

// Generate sends a normalized ChatRequest via OpenAI Chat Completions API.
func (c *CompatProvider) Generate(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if strings.TrimSpace(req.Config.Model) == "" {
		return nil, fmt.Errorf("openai_compat: model is required")
	}

	msgs, err := buildCompatMessages(req.SystemPrompt, req.Messages)
	if err != nil {
		return nil, fmt.Errorf("openai_compat: build messages: %w", err)
	}

	params := openaisdk.ChatCompletionNewParams{
		Model:    shared.ChatModel(req.Config.Model),
		Messages: msgs,
	}
	if req.Config.MaxOutputTokens > 0 {
		params.MaxCompletionTokens = openaisdk.Int(int64(req.Config.MaxOutputTokens))
	}
	if req.Config.Temperature != nil {
		params.Temperature = openaisdk.Float(*req.Config.Temperature)
	}
	if effort := mapReasoningEffort(req.Config.ReasoningEffort); effort != "" {
		params.ReasoningEffort = effort
	}

	if len(req.Tools) > 0 {
		tools := make([]openaisdk.ChatCompletionToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			schemaMap, err := llm.SchemaToMap(t.Parameters)
			if err != nil {
				return nil, fmt.Errorf("openai_compat: tool %q schema: %w", t.Name, err)
			}
			fn := shared.FunctionDefinitionParam{
				Name:       t.Name,
				Parameters: shared.FunctionParameters(schemaMap),
			}
			if t.Description != "" {
				fn.Description = openaisdk.String(t.Description)
			}
			tools = append(tools, openaisdk.ChatCompletionFunctionTool(fn))
		}
		params.Tools = tools
	}

	if req.ToolChoice != "" {
		params.ToolChoice = mapCompatToolChoice(req.ToolChoice)
	}

	if req.ResponseSchema != nil {
		schemaMap, err := llm.SchemaToMap(req.ResponseSchema)
		if err != nil {
			return nil, fmt.Errorf("openai_compat: response_schema: %w", err)
		}
		params.ResponseFormat = openaisdk.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "output_schema",
					Schema: schemaMap,
				},
			},
		}
	}

	client := c.base.clientForRequest(req.Config.BaseURL)
	resp, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openai_compat chat.completions.new: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("openai_compat: response contained no choices")
	}

	choice := resp.Choices[0]
	var toolCalls []llm.ToolCall
	for _, tc := range choice.Message.ToolCalls {
		if tc.Type != "function" && tc.Function.Name == "" {
			continue
		}
		fnCall := tc.AsFunction()
		args := json.RawMessage(strings.TrimSpace(fnCall.Function.Arguments))
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		toolCalls = append(toolCalls, llm.ToolCall{
			ID:        fnCall.ID,
			Name:      fnCall.Function.Name,
			Arguments: args,
		})
	}

	finishReason := choice.FinishReason
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if finishReason == "" {
		finishReason = "stop"
	}

	msg := llm.Message{
		Role:      llm.RoleAssistant,
		Content:   choice.Message.Content,
		ToolCalls: toolCalls,
	}

	return &llm.ChatResponse{
		Message:   msg,
		ToolCalls: toolCalls,
		Usage: llm.TokenUsage{
			InputTokens:  int(resp.Usage.PromptTokens),
			OutputTokens: int(resp.Usage.CompletionTokens),
			CachedTokens: int(resp.Usage.PromptTokensDetails.CachedTokens),
		},
		FinishReason: finishReason,
	}, nil
}

func buildCompatMessages(systemPrompt string, messages []llm.Message) ([]openaisdk.ChatCompletionMessageParamUnion, error) {
	out := make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	if systemPrompt != "" {
		out = append(out, openaisdk.SystemMessage(systemPrompt))
	}
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleSystem:
			out = append(out, openaisdk.SystemMessage(msg.Content))
		case llm.RoleUser:
			out = append(out, openaisdk.UserMessage(msg.Content))
		case llm.RoleAssistant:
			assistant := openaisdk.ChatCompletionAssistantMessageParam{}
			if msg.Content != "" {
				assistant.Content = openaisdk.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openaisdk.String(msg.Content),
				}
			}
			for _, tc := range msg.ToolCalls {
				args := strings.TrimSpace(string(tc.Arguments))
				if args == "" {
					args = "{}"
				}
				assistant.ToolCalls = append(assistant.ToolCalls, openaisdk.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openaisdk.ChatCompletionMessageFunctionToolCallParam{
						ID: tc.ID,
						Function: openaisdk.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: args,
						},
					},
				})
			}
			out = append(out, openaisdk.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
		case llm.RoleTool:
			out = append(out, openaisdk.ToolMessage(msg.Content, msg.ToolCallID))
		default:
			return nil, fmt.Errorf("unsupported message role %q", msg.Role)
		}
	}
	return out, nil
}

func mapCompatToolChoice(choice string) openaisdk.ChatCompletionToolChoiceOptionUnionParam {
	switch choice {
	case "auto", "required", "none":
		return openaisdk.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openaisdk.String(choice),
		}
	default:
		return openaisdk.ToolChoiceOptionFunctionToolChoice(openaisdk.ChatCompletionNamedToolChoiceFunctionParam{
			Name: choice,
		})
	}
}
