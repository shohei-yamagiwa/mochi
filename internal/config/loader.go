package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

var (
	// ErrCustomImagesDisallowed is returned when a repo override attempts to alter sandbox images while disabled.
	ErrCustomImagesDisallowed = errors.New("custom sandbox images are disallowed by repo_overrides.allow_custom_images=false")
	// ErrCustomPromptsDisallowed is returned when a repo override attempts to alter prompt files while disabled.
	ErrCustomPromptsDisallowed = errors.New("custom prompt overrides are disallowed by repo_overrides.allow_custom_prompts=false")
)

var validProviders = []string{"anthropic", "openai", "gemini", "openai_compat"}

// RepoOverride defines the subset of workflow fields that a target repository's .mochi/config.yaml may customize.
type RepoOverride struct {
	Input       *InputOverride       `yaml:"input,omitempty"`
	Sandbox     *SandboxOverride     `yaml:"sandbox,omitempty"`
	Agents      []AgentRoleOverride  `yaml:"agents,omitempty"`
	Synthesizer *SynthesizerOverride `yaml:"synthesizer,omitempty"`
	Output      *OutputOverride      `yaml:"output,omitempty"`
}

// InputOverride overrides trigger settings.
type InputOverride struct {
	Triggers *domain.TriggerConfig `yaml:"triggers,omitempty"`
}

// SandboxOverride captures sandbox override attempts from a target repository.
type SandboxOverride struct {
	Provider        string `yaml:"provider,omitempty"`
	DefaultSnapshot string `yaml:"default_snapshot,omitempty"`
	DefaultImage    string `yaml:"default_image,omitempty"`
}

// AgentRoleOverride customizes an existing role by name.
type AgentRoleOverride struct {
	Role       string               `yaml:"role"`
	Enabled    *bool                `yaml:"enabled,omitempty"`
	PromptFile string               `yaml:"prompt_file,omitempty"`
	LLM        *domain.ModelConfig  `yaml:"llm,omitempty"`
	Tools      []string             `yaml:"tools,omitempty"`
	Budgets    *domain.BudgetConfig `yaml:"budgets,omitempty"`
}

// SynthesizerOverride customizes synthesizer prompt or LLM settings.
type SynthesizerOverride struct {
	PromptFile string               `yaml:"prompt_file,omitempty"`
	LLM        *domain.ModelConfig  `yaml:"llm,omitempty"`
	Budgets    *domain.BudgetConfig `yaml:"budgets,omitempty"`
}

// OutputOverride customizes output sinks.
type OutputOverride struct {
	Sinks []domain.SinkConfig `yaml:"sinks,omitempty"`
}

// LoadWorkflowFile reads, parses, and validates a declarative workflow YAML file.
// baseDir is used to resolve relative prompt_file paths; if empty, the workflow file's working-directory-relative path is checked as-is.
func LoadWorkflowFile(path string, baseDir string) (*domain.Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow file %q: %w", path, err)
	}
	wf, err := ParseWorkflow(data)
	if err != nil {
		return nil, fmt.Errorf("parse workflow file %q: %w", path, err)
	}
	if err := ValidateWorkflow(wf, baseDir); err != nil {
		return nil, fmt.Errorf("validate workflow file %q: %w", path, err)
	}
	return wf, nil
}

// ParseWorkflow decodes YAML bytes into a domain.Workflow using goccy/go-yaml.
func ParseWorkflow(data []byte) (*domain.Workflow, error) {
	var wf domain.Workflow
	if err := yaml.UnmarshalWithOptions(data, &wf, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("unmarshal workflow yaml: %w", err)
	}
	return &wf, nil
}

// MarshalWorkflowYAML serializes a resolved Workflow back to YAML for audit persistence.
func MarshalWorkflowYAML(wf *domain.Workflow) (string, error) {
	if wf == nil {
		return "", fmt.Errorf("cannot marshal nil workflow")
	}
	data, err := yaml.Marshal(wf)
	if err != nil {
		return "", fmt.Errorf("marshal workflow yaml: %w", err)
	}
	return string(data), nil
}

// ValidateWorkflow validates workflow structure, provider/budget sanity, and prompt file references.
func ValidateWorkflow(wf *domain.Workflow, baseDir string) error {
	if wf == nil {
		return fmt.Errorf("workflow must not be nil")
	}
	if strings.TrimSpace(wf.Version) == "" {
		return fmt.Errorf("workflow version is required")
	}
	if strings.TrimSpace(wf.Name) == "" {
		return fmt.Errorf("workflow name is required")
	}
	if strings.TrimSpace(wf.Input.Adapter) == "" {
		return fmt.Errorf("input.adapter is required")
	}
	if strings.TrimSpace(wf.Sandbox.Provider) == "" {
		return fmt.Errorf("sandbox.provider is required")
	}
	if len(wf.Agents) == 0 {
		return fmt.Errorf("at least one specialist agent is required")
	}

	seenRoles := make(map[string]struct{}, len(wf.Agents))
	for i, agent := range wf.Agents {
		role := strings.TrimSpace(agent.Role)
		if role == "" {
			return fmt.Errorf("agents[%d].role is required", i)
		}
		if _, dup := seenRoles[role]; dup {
			return fmt.Errorf("duplicate agent role %q", role)
		}
		seenRoles[role] = struct{}{}

		if err := validatePromptPath(agent.PromptFile, baseDir); err != nil {
			return fmt.Errorf("agents[%s].prompt_file: %w", role, err)
		}
		if err := validateModelConfig(agent.LLM); err != nil {
			return fmt.Errorf("agents[%s].llm: %w", role, err)
		}
		if len(agent.Tools) == 0 {
			return fmt.Errorf("agents[%s].tools must not be empty", role)
		}
		if err := validateBudgetConfig(agent.Budgets); err != nil {
			return fmt.Errorf("agents[%s].budgets: %w", role, err)
		}
	}

	if err := validatePromptPath(wf.Synthesizer.PromptFile, baseDir); err != nil {
		return fmt.Errorf("synthesizer.prompt_file: %w", err)
	}
	if err := validateModelConfig(wf.Synthesizer.LLM); err != nil {
		return fmt.Errorf("synthesizer.llm: %w", err)
	}
	if err := validateBudgetConfig(wf.Synthesizer.Budgets); err != nil {
		return fmt.Errorf("synthesizer.budgets: %w", err)
	}

	if len(wf.Output.Sinks) == 0 {
		return fmt.Errorf("at least one output sink is required")
	}
	for i, s := range wf.Output.Sinks {
		if strings.TrimSpace(s.Type) == "" {
			return fmt.Errorf("output.sinks[%d].type is required", i)
		}
		if s.MinConfidence < 0 || s.MinConfidence > 1 {
			return fmt.Errorf("output.sinks[%d].min_confidence must be between 0.0 and 1.0", i)
		}
	}

	return nil
}

// MergeRepoOverrides applies repository-level .mochi/config.yaml overrides onto a cloned base workflow
// while enforcing security controls (allow_custom_images, allow_custom_prompts).
func MergeRepoOverrides(base *domain.Workflow, overrideYAML []byte) (*domain.Workflow, error) {
	if base == nil {
		return nil, fmt.Errorf("base workflow must not be nil")
	}

	cloned, err := cloneWorkflow(base)
	if err != nil {
		return nil, err
	}
	if !cloned.RepoOverrides.Enabled || len(strings.TrimSpace(string(overrideYAML))) == 0 {
		return cloned, nil
	}

	var ov RepoOverride
	if err := yaml.UnmarshalWithOptions(overrideYAML, &ov, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("unmarshal repo override yaml: %w", err)
	}

	if ov.Sandbox != nil {
		if !cloned.RepoOverrides.AllowCustomImages &&
			(ov.Sandbox.DefaultImage != "" || ov.Sandbox.DefaultSnapshot != "" || ov.Sandbox.Provider != "") {
			return nil, ErrCustomImagesDisallowed
		}
		if ov.Sandbox.Provider != "" {
			cloned.Sandbox.Provider = ov.Sandbox.Provider
		}
		if ov.Sandbox.DefaultSnapshot != "" {
			cloned.Sandbox.DefaultSnapshot = ov.Sandbox.DefaultSnapshot
		}
		if ov.Sandbox.DefaultImage != "" {
			cloned.Sandbox.DefaultImage = ov.Sandbox.DefaultImage
		}
	}

	if ov.Input != nil && ov.Input.Triggers != nil {
		if len(ov.Input.Triggers.Events) > 0 {
			cloned.Input.Triggers.Events = slices.Clone(ov.Input.Triggers.Events)
		}
		if ov.Input.Triggers.CommentCommand != "" {
			cloned.Input.Triggers.CommentCommand = ov.Input.Triggers.CommentCommand
		}
		cloned.Input.Triggers.IgnoreDrafts = ov.Input.Triggers.IgnoreDrafts
	}

	if len(ov.Agents) > 0 {
		roleIndex := make(map[string]int, len(cloned.Agents))
		for i, a := range cloned.Agents {
			roleIndex[a.Role] = i
		}

		disabled := make(map[string]bool)
		for _, aov := range ov.Agents {
			if aov.PromptFile != "" && !cloned.RepoOverrides.AllowCustomPrompts {
				return nil, fmt.Errorf("agent %q: %w", aov.Role, ErrCustomPromptsDisallowed)
			}
			idx, exists := roleIndex[aov.Role]
			if !exists {
				return nil, fmt.Errorf("repo override references unknown agent role %q", aov.Role)
			}
			if aov.Enabled != nil && !*aov.Enabled {
				disabled[aov.Role] = true
				continue
			}
			if aov.PromptFile != "" {
				if err := ensureSafeRelativePath(aov.PromptFile); err != nil {
					return nil, fmt.Errorf("agent %q prompt_file: %w", aov.Role, err)
				}
				cloned.Agents[idx].PromptFile = aov.PromptFile
			}
			if aov.LLM != nil {
				mergeModelConfig(&cloned.Agents[idx].LLM, aov.LLM)
			}
			if len(aov.Tools) > 0 {
				cloned.Agents[idx].Tools = slices.Clone(aov.Tools)
			}
			if aov.Budgets != nil {
				mergeBudgetConfig(&cloned.Agents[idx].Budgets, aov.Budgets)
			}
		}

		if len(disabled) > 0 {
			filtered := make([]domain.RoleConfig, 0, len(cloned.Agents))
			for _, a := range cloned.Agents {
				if !disabled[a.Role] {
					filtered = append(filtered, a)
				}
			}
			cloned.Agents = filtered
		}
	}

	if ov.Synthesizer != nil {
		if ov.Synthesizer.PromptFile != "" {
			if !cloned.RepoOverrides.AllowCustomPrompts {
				return nil, fmt.Errorf("synthesizer: %w", ErrCustomPromptsDisallowed)
			}
			if err := ensureSafeRelativePath(ov.Synthesizer.PromptFile); err != nil {
				return nil, fmt.Errorf("synthesizer prompt_file: %w", err)
			}
			cloned.Synthesizer.PromptFile = ov.Synthesizer.PromptFile
		}
		if ov.Synthesizer.LLM != nil {
			mergeModelConfig(&cloned.Synthesizer.LLM, ov.Synthesizer.LLM)
		}
		if ov.Synthesizer.Budgets != nil {
			mergeBudgetConfig(&cloned.Synthesizer.Budgets, ov.Synthesizer.Budgets)
		}
	}

	if ov.Output != nil && len(ov.Output.Sinks) > 0 {
		cloned.Output.Sinks = slices.Clone(ov.Output.Sinks)
	}

	return cloned, nil
}

func cloneWorkflow(wf *domain.Workflow) (*domain.Workflow, error) {
	raw, err := yaml.Marshal(wf)
	if err != nil {
		return nil, fmt.Errorf("clone workflow: %w", err)
	}
	return ParseWorkflow(raw)
}

func mergeModelConfig(dst, src *domain.ModelConfig) {
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.BaseURL != "" {
		dst.BaseURL = src.BaseURL
	}
	if src.Temperature != nil {
		t := *src.Temperature
		dst.Temperature = &t
	}
	if src.MaxOutputTokens > 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
	if src.ReasoningEffort != "" {
		dst.ReasoningEffort = src.ReasoningEffort
	}
}

func mergeBudgetConfig(dst, src *domain.BudgetConfig) {
	if src.MaxTurns > 0 {
		dst.MaxTurns = src.MaxTurns
	}
	if src.MaxTokens > 0 {
		dst.MaxTokens = src.MaxTokens
	}
	if src.CommandTimeout > 0 {
		dst.CommandTimeout = src.CommandTimeout
	}
	if src.RunTimeout > 0 {
		dst.RunTimeout = src.RunTimeout
	}
}

func validateModelConfig(m domain.ModelConfig) error {
	if strings.TrimSpace(m.Provider) == "" {
		return fmt.Errorf("provider is required")
	}
	if !slices.Contains(validProviders, m.Provider) {
		return fmt.Errorf("unsupported provider %q", m.Provider)
	}
	if strings.TrimSpace(m.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if m.Temperature != nil && (*m.Temperature < 0 || *m.Temperature > 2) {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	if m.MaxOutputTokens < 0 {
		return fmt.Errorf("max_output_tokens must not be negative")
	}
	if m.ReasoningEffort != "" {
		switch m.ReasoningEffort {
		case "minimal", "low", "medium", "high":
		default:
			return fmt.Errorf("unsupported reasoning_effort %q", m.ReasoningEffort)
		}
	}
	return nil
}

func validateBudgetConfig(b domain.BudgetConfig) error {
	if b.MaxTurns < 0 {
		return fmt.Errorf("max_turns must not be negative")
	}
	if b.MaxTokens < 0 {
		return fmt.Errorf("max_tokens must not be negative")
	}
	if b.CommandTimeout < 0 {
		return fmt.Errorf("command_timeout must not be negative")
	}
	if b.RunTimeout < 0 {
		return fmt.Errorf("run_timeout must not be negative")
	}
	return nil
}

func validatePromptPath(promptPath, baseDir string) error {
	if err := ensureSafeRelativePath(promptPath); err != nil {
		return err
	}
	if baseDir != "" {
		resolved := filepath.Join(baseDir, filepath.FromSlash(promptPath))
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("stat prompt file %q: %w", resolved, err)
		}
		if info.IsDir() {
			return fmt.Errorf("prompt path %q is a directory", resolved)
		}
	}
	return nil
}

func ensureSafeRelativePath(p string) error {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return fmt.Errorf("prompt_file is required")
	}
	if filepath.IsAbs(trimmed) {
		return fmt.Errorf("prompt_file %q must be a relative path", trimmed)
	}
	cleaned := filepath.Clean(filepath.FromSlash(trimmed))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("prompt_file %q escapes workspace root", trimmed)
	}
	return nil
}
