package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration wrapper that marshals and unmarshals human-readable duration strings (e.g. "60s", "5m").
type Duration time.Duration

// Std returns the underlying time.Duration value.
func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

// String implements fmt.Stringer.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// UnmarshalText parses a Go duration string such as "3m" or "60s", or an integer nanosecond string.
func (d *Duration) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*d = 0
		return nil
	}
	if parsed, err := time.ParseDuration(s); err == nil {
		*d = Duration(parsed)
		return nil
	}
	if ns, err := strconv.ParseInt(s, 10, 64); err == nil {
		*d = Duration(ns)
		return nil
	}
	return fmt.Errorf("invalid duration %q", s)
}

// MarshalText formats the duration as a Go duration string.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalJSON parses a JSON string duration (e.g. "60s") or numeric nanoseconds.
func (d *Duration) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" || s == "" {
		*d = 0
		return nil
	}
	if strings.HasPrefix(s, "\"") {
		var str string
		if err := json.Unmarshal(data, &str); err != nil {
			return err
		}
		return d.UnmarshalText([]byte(str))
	}
	var ns int64
	if err := json.Unmarshal(data, &ns); err != nil {
		return err
	}
	*d = Duration(ns)
	return nil
}

// MarshalJSON encodes the duration as a JSON string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalYAML implements goccy/go-yaml BytesUnmarshaler.
func (d *Duration) UnmarshalYAML(data []byte) error {
	s := strings.TrimSpace(string(data))
	s = strings.Trim(s, `"'`)
	return d.UnmarshalText([]byte(s))
}

// MarshalYAML implements goccy/go-yaml InterfaceMarshaler.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// ModelConfig configures an LLM provider, model, and inference options for a role.
type ModelConfig struct {
	Provider        string   `yaml:"provider" json:"provider"`                     // "anthropic", "openai", "gemini", "openai_compat"
	Model           string   `yaml:"model" json:"model"`                           // e.g. "claude-sonnet-5-5", "gpt-6.1-sol", "gemini-3.8-flash"
	BaseURL         string   `yaml:"base_url,omitempty" json:"base_url,omitempty"` // Optional base URL for openai_compat endpoints
	Temperature     *float64 `yaml:"temperature,omitempty" json:"temperature,omitempty"`
	MaxOutputTokens int      `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	ReasoningEffort string   `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"` // "minimal", "low", "medium", "high"
}

// Workflow represents a complete declarative workflow definition (e.g. pr-review.yaml).
type Workflow struct {
	Version       string              `yaml:"version" json:"version"`
	Name          string              `yaml:"name" json:"name"`
	Description   string              `yaml:"description,omitempty" json:"description,omitempty"`
	Input         InputConfig         `yaml:"input" json:"input"`
	RepoOverrides RepoOverridesConfig `yaml:"repo_overrides,omitempty" json:"repo_overrides,omitempty"`
	Sandbox       SandboxConfig       `yaml:"sandbox" json:"sandbox"`
	Agents        []RoleConfig        `yaml:"agents" json:"agents"`
	Synthesizer   SynthesizerConfig   `yaml:"synthesizer" json:"synthesizer"`
	Output        OutputConfig        `yaml:"output" json:"output"`
}

// InputConfig defines how triggers are ingested for a workflow.
type InputConfig struct {
	Adapter  string        `yaml:"adapter" json:"adapter"`
	Triggers TriggerConfig `yaml:"triggers,omitempty" json:"triggers,omitempty"`
}

// TriggerConfig defines trigger filtering rules for an input adapter.
type TriggerConfig struct {
	Events                     []string `yaml:"events,omitempty" json:"events,omitempty"`
	CommentCommand             string   `yaml:"comment_command,omitempty" json:"comment_command,omitempty"`
	AllowedCommentAssociations []string `yaml:"allowed_comment_associations,omitempty" json:"allowed_comment_associations,omitempty"`
	IgnoreDrafts               bool     `yaml:"ignore_drafts,omitempty" json:"ignore_drafts,omitempty"`
}

// RepoOverridesConfig governs whether target repositories may override workflow behavior via .mochi/.
type RepoOverridesConfig struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	Path               string `yaml:"path,omitempty" json:"path,omitempty"`
	AllowCustomPrompts bool   `yaml:"allow_custom_prompts,omitempty" json:"allow_custom_prompts,omitempty"`
	AllowCustomImages  bool   `yaml:"allow_custom_images,omitempty" json:"allow_custom_images,omitempty"`
}

// SandboxConfig defines base sandbox provisioning settings.
type SandboxConfig struct {
	Provider        string   `yaml:"provider" json:"provider"`
	DefaultSnapshot string   `yaml:"default_snapshot,omitempty" json:"default_snapshot,omitempty"`
	DefaultImage    string   `yaml:"default_image,omitempty" json:"default_image,omitempty"`
	ForkFromBase    bool     `yaml:"fork_from_base,omitempty" json:"fork_from_base,omitempty"`
	SetupTimeout    Duration `yaml:"setup_timeout,omitempty" json:"setup_timeout,omitempty"`
}

// RoleConfig defines an individual specialist agent role in a workflow.
type RoleConfig struct {
	Role           string             `yaml:"role" json:"role"`
	PromptFile     string             `yaml:"prompt_file" json:"prompt_file"`
	PromptFromRepo bool               `yaml:"prompt_from_repo,omitempty" json:"prompt_from_repo,omitempty"`
	LLM            ModelConfig        `yaml:"llm" json:"llm"`
	Sandbox        AgentSandboxConfig `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	Tools          []string           `yaml:"tools" json:"tools"`
	Budgets        BudgetConfig       `yaml:"budgets,omitempty" json:"budgets,omitempty"`
}

// AgentSandboxConfig controls whether a specialist role runs with a sandbox VM.
type AgentSandboxConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// BudgetConfig defines execution guardrails for an agent or synthesizer run.
type BudgetConfig struct {
	MaxTurns       int      `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxTokens      int      `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	CommandTimeout Duration `yaml:"command_timeout,omitempty" json:"command_timeout,omitempty"`
	RunTimeout     Duration `yaml:"run_timeout,omitempty" json:"run_timeout,omitempty"`
}

// SynthesizerConfig defines the final synthesis agent step.
type SynthesizerConfig struct {
	PromptFile     string       `yaml:"prompt_file" json:"prompt_file"`
	PromptFromRepo bool         `yaml:"prompt_from_repo,omitempty" json:"prompt_from_repo,omitempty"`
	LLM            ModelConfig  `yaml:"llm" json:"llm"`
	Budgets        BudgetConfig `yaml:"budgets,omitempty" json:"budgets,omitempty"`
}

// OutputConfig defines one or more output sinks for delivering the final artifact.
type OutputConfig struct {
	Sinks []SinkConfig `yaml:"sinks" json:"sinks"`
}

// SinkConfig configures a single output sink instance.
type SinkConfig struct {
	Type                   string         `yaml:"type" json:"type"`
	PostInlineComments     bool           `yaml:"post_inline_comments,omitempty" json:"post_inline_comments,omitempty"`
	IncludeCoverageSummary bool           `yaml:"include_coverage_summary,omitempty" json:"include_coverage_summary,omitempty"`
	MinSeverity            string         `yaml:"min_severity,omitempty" json:"min_severity,omitempty"`
	MinConfidence          float64        `yaml:"min_confidence,omitempty" json:"min_confidence,omitempty"`
	VerdictMode            string         `yaml:"verdict_mode,omitempty" json:"verdict_mode,omitempty"`
	URL                    string         `yaml:"url,omitempty" json:"url,omitempty"`
	Options                map[string]any `yaml:"options,omitempty" json:"options,omitempty"`
}
