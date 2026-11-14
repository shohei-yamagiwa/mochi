package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

// ModelConfig aliases domain.ModelConfig for LLM provider configuration.
type ModelConfig = domain.ModelConfig

// Role identifies the speaker of a chat message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message represents a single normalized conversation message across LLM providers.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolDefinition declares a function tool and its JSON Schema parameters.
type ToolDefinition struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  *jsonschema.Schema `json:"parameters"`
}

// ToolCall represents a tool invocation emitted by an LLM.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// TokenUsage records token consumption for a model turn.
type TokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// ChatRequest encapsulates a normalized inference request sent to an LLM Provider.
type ChatRequest struct {
	Config         ModelConfig        `json:"config"`
	SystemPrompt   string             `json:"system_prompt"`
	Messages       []Message          `json:"messages"`
	Tools          []ToolDefinition   `json:"tools,omitempty"`
	ToolChoice     string             `json:"tool_choice,omitempty"` // "auto", "required", or specific tool name (e.g. "submit_output")
	ResponseSchema *jsonschema.Schema `json:"response_schema,omitempty"`
}

// ChatResponse encapsulates a normalized inference response from an LLM Provider.
type ChatResponse struct {
	Message      Message    `json:"message"`
	Thinking     string     `json:"thinking,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	Usage        TokenUsage `json:"usage"`
	FinishReason string     `json:"finish_reason"`
}

// Provider abstracts a native LLM SDK (OpenAI, Anthropic, Gemini, or OpenAI-compatible).
type Provider interface {
	Name() string
	Generate(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// Registry stores named LLM Provider implementations.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

// NewRegistry creates an empty LLM provider registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Provider),
	}
}

// Register adds a Provider to the registry under its Name().
func (r *Registry) Register(provider Provider) error {
	if provider == nil {
		return fmt.Errorf("cannot register nil llm provider")
	}
	name := provider.Name()
	if name == "" {
		return fmt.Errorf("llm provider name must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("llm provider %q already registered", name)
	}
	r.providers[name] = provider
	return nil
}

// Get retrieves a registered Provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}
