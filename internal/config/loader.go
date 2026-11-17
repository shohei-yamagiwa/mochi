package config

import (
	"errors"
	"fmt"
	"maps"
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
	// ErrDisallowedOverrideField is returned when a repo override attempts to modify a security-sensitive field.
	ErrDisallowedOverrideField = errors.New("repo override attempts to modify a disallowed field")
	// ErrDisallowedToolOverride is returned when a repo override attempts to grant a tool not permitted by the server workflow.
	ErrDisallowedToolOverride = errors.New("repo override attempts to grant a tool not in the base role allowlist")
)

var (
	validProviders  = []string{"anthropic", "openai", "gemini", "openai_compat"}
	validSeverities = []string{"critical", "high", "medium", "low", "nit"}
)

// RepoOverride defines the allowlisted workflow fields that a target repository's .mochi/config.yaml
// (loaded from the base branch) may customize.
type RepoOverride struct {
	Input       *InputOverride       `yaml:"input,omitempty"`
	Sandbox     *SandboxOverride     `yaml:"sandbox,omitempty"`
	Agents      []AgentRoleOverride  `yaml:"agents,omitempty"`
	Synthesizer *SynthesizerOverride `yaml:"synthesizer,omitempty"`
	Output      *OutputOverride      `yaml:"output,omitempty"`
}

// InputOverride overrides trigger settings.
type InputOverride struct {
	Triggers *TriggerOverride `yaml:"triggers,omitempty"`
}

// TriggerOverride uses pointer bools so unset fields preserve base workflow defaults.
type TriggerOverride struct {
	Events         []string `yaml:"events,omitempty"`
	CommentCommand string   `yaml:"comment_command,omitempty"`
	IgnoreDrafts   *bool    `yaml:"ignore_drafts,omitempty"`
}

// SandboxOverride captures sandbox override attempts from a target repository.
type SandboxOverride struct {
	Provider        string `yaml:"provider,omitempty"`
	DefaultSnapshot string `yaml:"default_snapshot,omitempty"`
	DefaultImage    string `yaml:"default_image,omitempty"`
}

// ModelOverride defines the safe subset of LLM settings a repo override may tune.
// Provider and BaseURL are captured only to reject disallowed override attempts explicitly.
// Model is rejected unless the server workflow sets repo_overrides.allow_custom_models.
type ModelOverride struct {
	Provider        string   `yaml:"provider,omitempty"`
	Model           string   `yaml:"model,omitempty"`
	BaseURL         string   `yaml:"base_url,omitempty"`
	Temperature     *float64 `yaml:"temperature,omitempty"`
	MaxOutputTokens int      `yaml:"max_output_tokens,omitempty"`
	ReasoningEffort string   `yaml:"reasoning_effort,omitempty"`
}

// AgentRoleOverride customizes an existing role by name within server-defined bounds.
type AgentRoleOverride struct {
	Role       string               `yaml:"role"`
	Enabled    *bool                `yaml:"enabled,omitempty"`
	PromptFile string               `yaml:"prompt_file,omitempty"`
	LLM        *ModelOverride       `yaml:"llm,omitempty"`
	Tools      []string             `yaml:"tools,omitempty"`
	Budgets    *domain.BudgetConfig `yaml:"budgets,omitempty"`
}

// SynthesizerOverride customizes synthesizer prompt or safe LLM/budget settings.
type SynthesizerOverride struct {
	PromptFile string               `yaml:"prompt_file,omitempty"`
	LLM        *ModelOverride       `yaml:"llm,omitempty"`
	Budgets    *domain.BudgetConfig `yaml:"budgets,omitempty"`
}

// OutputOverride tunes existing output sinks without allowing new sinks or external webhook URLs.
type OutputOverride struct {
	Sinks []SinkOverride `yaml:"sinks,omitempty"`
}

// SinkOverride defines safe tuning knobs for an existing output sink.
type SinkOverride struct {
	Type                   string   `yaml:"type"`
	PostInlineComments     *bool    `yaml:"post_inline_comments,omitempty"`
	IncludeCoverageSummary *bool    `yaml:"include_coverage_summary,omitempty"`
	MinSeverity            string   `yaml:"min_severity,omitempty"`
	MinConfidence          *float64 `yaml:"min_confidence,omitempty"`
	URL                    string   `yaml:"url,omitempty"`
}

// LoadWorkflowFile reads, parses, and validates a declarative workflow YAML file.
// baseDir is used to resolve relative prompt_file paths; if empty, only structural validation is performed.
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

// ParseWorkflow decodes server workflow YAML bytes into a domain.Workflow using goccy/go-yaml.
// It rejects prompt_from_repo=true since server configs must not mark prompts as repo-owned.
func ParseWorkflow(data []byte) (*domain.Workflow, error) {
	wf, err := ParseResolvedWorkflow(data)
	if err != nil {
		return nil, err
	}
	for _, a := range wf.Agents {
		if a.PromptFromRepo {
			return nil, fmt.Errorf("agent %q: prompt_from_repo cannot be set in server workflow YAML", a.Role)
		}
	}
	if wf.Synthesizer.PromptFromRepo {
		return nil, fmt.Errorf("synthesizer: prompt_from_repo cannot be set in server workflow YAML")
	}
	return wf, nil
}

// ParseRepoWorkflow decodes a repository-owned workflow YAML (e.g. .mochi/workflows/*.yaml)
// fetched from the base branch. Prompt paths under .mochi/ are marked PromptFromRepo=true
// so PromptEngine reads them from the repository bundle; non-.mochi/ paths must match built-in
// embedded prompts.
func ParseRepoWorkflow(data []byte) (*domain.Workflow, error) {
	wf, err := ParseWorkflow(data)
	if err != nil {
		return nil, err
	}
	for i := range wf.Agents {
		cleanSlash := filepath.ToSlash(filepath.Clean(filepath.FromSlash(wf.Agents[i].PromptFile)))
		if strings.HasPrefix(cleanSlash, ".mochi/") {
			if err := ensureSafeRepoPromptPath(wf.Agents[i].PromptFile); err != nil {
				return nil, fmt.Errorf("agent %q prompt_file: %w", wf.Agents[i].Role, err)
			}
			wf.Agents[i].PromptFile = cleanSlash
			wf.Agents[i].PromptFromRepo = true
		} else {
			if _, err := ReadEmbeddedPrompt(wf.Agents[i].PromptFile); err != nil {
				return nil, fmt.Errorf("agent %q prompt_file %q must be under .mochi/ or match a built-in prompt: %w", wf.Agents[i].Role, wf.Agents[i].PromptFile, err)
			}
			wf.Agents[i].PromptFromRepo = false
		}
	}
	cleanSynth := filepath.ToSlash(filepath.Clean(filepath.FromSlash(wf.Synthesizer.PromptFile)))
	if strings.HasPrefix(cleanSynth, ".mochi/") {
		if err := ensureSafeRepoPromptPath(wf.Synthesizer.PromptFile); err != nil {
			return nil, fmt.Errorf("synthesizer prompt_file: %w", err)
		}
		wf.Synthesizer.PromptFile = cleanSynth
		wf.Synthesizer.PromptFromRepo = true
	} else {
		if _, err := ReadEmbeddedPrompt(wf.Synthesizer.PromptFile); err != nil {
			return nil, fmt.Errorf("synthesizer prompt_file %q must be under .mochi/ or match a built-in prompt: %w", wf.Synthesizer.PromptFile, err)
		}
		wf.Synthesizer.PromptFromRepo = false
	}
	if err := ValidateWorkflow(wf, ""); err != nil {
		return nil, err
	}
	return wf, nil
}

// ParseResolvedWorkflow decodes a persisted resolved_config_yaml snapshot (which may contain prompt_from_repo).
func ParseResolvedWorkflow(data []byte) (*domain.Workflow, error) {
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

		promptBase := baseDir
		if agent.PromptFromRepo {
			promptBase = ""
		}
		if err := validatePromptPath(agent.PromptFile, promptBase); err != nil {
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

	synthBase := baseDir
	if wf.Synthesizer.PromptFromRepo {
		synthBase = ""
	}
	if err := validatePromptPath(wf.Synthesizer.PromptFile, synthBase); err != nil {
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
		if s.MinSeverity != "" && !slices.Contains(validSeverities, s.MinSeverity) {
			return fmt.Errorf("output.sinks[%d].min_severity %q is invalid", i, s.MinSeverity)
		}
		if s.MinConfidence < 0 || s.MinConfidence > 1 {
			return fmt.Errorf("output.sinks[%d].min_confidence must be between 0.0 and 1.0", i)
		}
	}

	return nil
}

// MergeRepoOverrides applies repository-level .mochi/config.yaml overrides (fetched from the base branch)
// onto a cloned base workflow while enforcing an explicit allowlist of safe fields and re-validating the result.
func MergeRepoOverrides(base *domain.Workflow, overrideYAML []byte) (*domain.Workflow, error) {
	if base == nil {
		return nil, fmt.Errorf("base workflow must not be nil")
	}

	cloned := cloneWorkflow(base)
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
		return nil, fmt.Errorf("input.triggers: %w (webhook triggers are evaluated before repository overrides are loaded)", ErrDisallowedOverrideField)
	}

	if len(ov.Agents) > 0 {
		roleIndex := make(map[string]int, len(cloned.Agents))
		for i, a := range cloned.Agents {
			roleIndex[a.Role] = i
		}

		disabled := make(map[string]bool)
		for _, aov := range ov.Agents {
			idx, exists := roleIndex[aov.Role]
			if !exists {
				return nil, fmt.Errorf("repo override references unknown agent role %q", aov.Role)
			}
			if aov.Enabled != nil && !*aov.Enabled {
				disabled[aov.Role] = true
				continue
			}
			if aov.PromptFile != "" {
				if !cloned.RepoOverrides.AllowCustomPrompts {
					return nil, fmt.Errorf("agent %q: %w", aov.Role, ErrCustomPromptsDisallowed)
				}
				if err := ensureSafeRepoPromptPath(aov.PromptFile); err != nil {
					return nil, fmt.Errorf("agent %q prompt_file: %w", aov.Role, err)
				}
				cloned.Agents[idx].PromptFile = filepath.ToSlash(filepath.Clean(filepath.FromSlash(aov.PromptFile)))
				cloned.Agents[idx].PromptFromRepo = true
			}
			if aov.LLM != nil {
				if err := mergeModelOverride(&cloned.Agents[idx].LLM, aov.LLM, cloned.RepoOverrides.AllowCustomModels); err != nil {
					return nil, fmt.Errorf("agent %q llm: %w", aov.Role, err)
				}
			}
			if len(aov.Tools) > 0 {
				allowed := cloned.Agents[idx].Tools
				for _, t := range aov.Tools {
					if !slices.Contains(allowed, t) {
						return nil, fmt.Errorf("agent %q tool %q: %w", aov.Role, t, ErrDisallowedToolOverride)
					}
				}
				cloned.Agents[idx].Tools = slices.Clone(aov.Tools)
			}
			if aov.Budgets != nil {
				if err := mergeBudgetConfig(&cloned.Agents[idx].Budgets, aov.Budgets); err != nil {
					return nil, fmt.Errorf("agent %q budgets: %w", aov.Role, err)
				}
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
			if err := ensureSafeRepoPromptPath(ov.Synthesizer.PromptFile); err != nil {
				return nil, fmt.Errorf("synthesizer prompt_file: %w", err)
			}
			cloned.Synthesizer.PromptFile = filepath.ToSlash(filepath.Clean(filepath.FromSlash(ov.Synthesizer.PromptFile)))
			cloned.Synthesizer.PromptFromRepo = true
		}
		if ov.Synthesizer.LLM != nil {
			if err := mergeModelOverride(&cloned.Synthesizer.LLM, ov.Synthesizer.LLM, cloned.RepoOverrides.AllowCustomModels); err != nil {
				return nil, fmt.Errorf("synthesizer llm: %w", err)
			}
		}
		if ov.Synthesizer.Budgets != nil {
			if err := mergeBudgetConfig(&cloned.Synthesizer.Budgets, ov.Synthesizer.Budgets); err != nil {
				return nil, fmt.Errorf("synthesizer budgets: %w", err)
			}
		}
	}

	if ov.Output != nil && len(ov.Output.Sinks) > 0 {
		sinkIndex := make(map[string]int, len(cloned.Output.Sinks))
		for i, s := range cloned.Output.Sinks {
			sinkIndex[s.Type] = i
		}
		for _, sov := range ov.Output.Sinks {
			if sov.URL != "" {
				return nil, fmt.Errorf("output sink %q url: %w", sov.Type, ErrDisallowedOverrideField)
			}
			idx, ok := sinkIndex[sov.Type]
			if !ok {
				return nil, fmt.Errorf("output sink %q not configured on server: %w", sov.Type, ErrDisallowedOverrideField)
			}
			if sov.PostInlineComments != nil {
				cloned.Output.Sinks[idx].PostInlineComments = *sov.PostInlineComments
			}
			if sov.IncludeCoverageSummary != nil {
				cloned.Output.Sinks[idx].IncludeCoverageSummary = *sov.IncludeCoverageSummary
			}
			if sov.MinSeverity != "" {
				cloned.Output.Sinks[idx].MinSeverity = sov.MinSeverity
			}
			if sov.MinConfidence != nil {
				cloned.Output.Sinks[idx].MinConfidence = *sov.MinConfidence
			}
		}
	}

	if err := ValidateWorkflow(cloned, ""); err != nil {
		return nil, fmt.Errorf("validate merged workflow: %w", err)
	}

	return cloned, nil
}

func cloneWorkflow(wf *domain.Workflow) *domain.Workflow {
	out := *wf
	out.Input.Triggers.Events = slices.Clone(wf.Input.Triggers.Events)
	out.Input.Triggers.BaseBranches = slices.Clone(wf.Input.Triggers.BaseBranches)
	out.Input.Triggers.HeadBranches = slices.Clone(wf.Input.Triggers.HeadBranches)
	out.Input.Triggers.AllowedCommentAssociations = slices.Clone(wf.Input.Triggers.AllowedCommentAssociations)
	out.Agents = make([]domain.RoleConfig, len(wf.Agents))
	for i, a := range wf.Agents {
		clonedAgent := a
		clonedAgent.Tools = slices.Clone(a.Tools)
		if a.LLM.Temperature != nil {
			t := *a.LLM.Temperature
			clonedAgent.LLM.Temperature = &t
		}
		out.Agents[i] = clonedAgent
	}
	if wf.Synthesizer.LLM.Temperature != nil {
		t := *wf.Synthesizer.LLM.Temperature
		out.Synthesizer.LLM.Temperature = &t
	}
	out.Output.Sinks = make([]domain.SinkConfig, len(wf.Output.Sinks))
	for i, s := range wf.Output.Sinks {
		clonedSink := s
		if s.Options != nil {
			clonedSink.Options = maps.Clone(s.Options)
		}
		out.Output.Sinks[i] = clonedSink
	}
	return &out
}

const defaultMaxOutputTokensCeiling = 16384

func reasoningEffortRank(effort string) (int, bool) {
	switch effort {
	case "minimal":
		return 1, true
	case "low":
		return 2, true
	case "medium":
		return 3, true
	case "high":
		return 4, true
	case "xhigh":
		return 5, true
	case "max":
		return 6, true
	default:
		return 0, false
	}
}

func mergeModelOverride(dst *domain.ModelConfig, src *ModelOverride, allowCustomModels bool) error {
	if src.BaseURL != "" {
		return fmt.Errorf("base_url: %w", ErrDisallowedOverrideField)
	}
	if src.Provider != "" {
		return fmt.Errorf("provider: %w", ErrDisallowedOverrideField)
	}
	if src.Model != "" {
		if !allowCustomModels {
			return fmt.Errorf("model: %w", ErrDisallowedOverrideField)
		}
		dst.Model = src.Model
	}
	if src.Temperature != nil {
		t := *src.Temperature
		dst.Temperature = &t
	}
	if src.MaxOutputTokens > 0 {
		dst.MaxOutputTokens = clampInt(src.MaxOutputTokens, dst.MaxOutputTokens, defaultMaxOutputTokensCeiling)
	}
	if src.ReasoningEffort != "" {
		if _, ok := reasoningEffortRank(src.ReasoningEffort); !ok {
			return fmt.Errorf("unsupported reasoning_effort %q", src.ReasoningEffort)
		}
		dst.ReasoningEffort = src.ReasoningEffort
	}
	return nil
}

func mergeBudgetConfig(dst, src *domain.BudgetConfig) error {
	if err := validateBudgetConfig(*src); err != nil {
		return err
	}
	if src.RunTimeout > 0 {
		dst.RunTimeout = src.RunTimeout
	}
	return nil
}

func clampInt(requested, base, ceiling int) int {
	limit := base
	if limit <= 0 {
		limit = ceiling
	}
	if limit > 0 && requested > limit {
		return limit
	}
	return requested
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
		if _, ok := reasoningEffortRank(m.ReasoningEffort); !ok {
			return fmt.Errorf("unsupported reasoning_effort %q", m.ReasoningEffort)
		}
	}
	return nil
}

func validateBudgetConfig(b domain.BudgetConfig) error {
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
		root, err := os.OpenRoot(baseDir)
		if err != nil {
			if _, embErr := ReadEmbeddedPrompt(promptPath); embErr == nil {
				return nil
			}
			return fmt.Errorf("open prompt root %q: %w", baseDir, err)
		}
		defer root.Close()

		cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(promptPath)))
		info, err := root.Stat(cleanRel)
		if err != nil {
			if _, embErr := ReadEmbeddedPrompt(promptPath); embErr == nil {
				return nil
			}
			return fmt.Errorf("stat prompt file %q under %q: %w", cleanRel, baseDir, err)
		}
		if info.IsDir() {
			return fmt.Errorf("prompt path %q is a directory", cleanRel)
		}
	}
	return nil
}

func ensureSafeRepoPromptPath(p string) error {
	if err := ensureSafeRelativePath(p); err != nil {
		return err
	}
	cleanSlash := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if !strings.HasPrefix(cleanSlash, ".mochi/") || len(cleanSlash) <= len(".mochi/") {
		return fmt.Errorf("repo prompt_file %q must be a file under .mochi/", p)
	}
	return nil
}

func ensureSafeRelativePath(p string) error {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return fmt.Errorf("prompt_file is required")
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return fmt.Errorf("prompt_file %q must be a relative path", trimmed)
	}
	cleaned := filepath.Clean(filepath.FromSlash(trimmed))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("prompt_file %q escapes workspace root", trimmed)
	}
	return nil
}
