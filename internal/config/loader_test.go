package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/config"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Clean(filepath.Join("..", ".."))
}

func TestLoadDefaultPRReviewWorkflow(t *testing.T) {
	root := repoRoot(t)
	wfPath := filepath.Join(root, "configs", "workflows", "pr-review.yaml")

	wf, err := config.LoadWorkflowFile(wfPath, root)
	if err != nil {
		t.Fatalf("LoadWorkflowFile failed: %v", err)
	}

	if wf.Name != "pr-review" {
		t.Fatalf("got workflow name %q, want pr-review", wf.Name)
	}
	if wf.Sandbox.SetupTimeout.Std() != 3*time.Minute {
		t.Fatalf("got setup_timeout %v, want 3m", wf.Sandbox.SetupTimeout.Std())
	}
	if len(wf.Agents) != 5 {
		t.Fatalf("got %d agents, want 5", len(wf.Agents))
	}
	if wf.Agents[3].Role != "maintainability" {
		t.Fatalf("unexpected maintainability config: %+v", wf.Agents[3])
	}
	if wf.Agents[4].Role != "test-runner" || wf.Agents[4].Budgets.RunTimeout.Std() != 30*time.Minute {
		t.Fatalf("unexpected test-runner config: %+v", wf.Agents[4])
	}
	if wf.Synthesizer.Budgets.RunTimeout.Std() != 2*time.Minute {
		t.Fatalf("unexpected synthesizer run_timeout: %v", wf.Synthesizer.Budgets.RunTimeout.Std())
	}

	yamlText, err := config.MarshalWorkflowYAML(wf)
	if err != nil {
		t.Fatalf("MarshalWorkflowYAML failed: %v", err)
	}
	if !strings.Contains(yamlText, "name: pr-review") {
		t.Fatalf("marshaled YAML missing workflow name:\n%s", yamlText)
	}
}

func TestMergeRepoOverridesSecurityControls(t *testing.T) {
	root := repoRoot(t)
	wf, err := config.LoadWorkflowFile(filepath.Join(root, "configs", "workflows", "pr-review.yaml"), root)
	if err != nil {
		t.Fatalf("LoadWorkflowFile failed: %v", err)
	}

	t.Run("rejects custom sandbox image when allow_custom_images is false", func(t *testing.T) {
		locked := *wf
		locked.RepoOverrides.AllowCustomImages = false
		override := []byte(`
sandbox:
  default_image: "evil/exfiltrate:latest"
`)
		_, err := config.MergeRepoOverrides(&locked, override)
		if !errors.Is(err, config.ErrCustomImagesDisallowed) {
			t.Fatalf("expected ErrCustomImagesDisallowed, got %v", err)
		}
	})

	t.Run("allows custom sandbox snapshot when allow_custom_images is true", func(t *testing.T) {
		if !wf.RepoOverrides.AllowCustomImages {
			t.Fatal("packaged pr-review workflow must allow custom images so owner-run repos can select their language snapshot")
		}
		override := []byte(`
sandbox:
  default_snapshot: "mochi-review-java"
`)
		merged, err := config.MergeRepoOverrides(wf, override)
		if err != nil {
			t.Fatalf("expected custom snapshot to be allowed, got %v", err)
		}
		if merged.Sandbox.DefaultSnapshot != "mochi-review-java" {
			t.Fatalf("got default_snapshot %q, want mochi-review-java", merged.Sandbox.DefaultSnapshot)
		}
	})

	t.Run("rejects custom prompt when allow_custom_prompts is false", func(t *testing.T) {
		locked := *wf
		locked.RepoOverrides.AllowCustomPrompts = false
		override := []byte(`
agents:
  - role: correctness
    prompt_file: ".mochi/prompts/custom.md"
`)
		_, err := config.MergeRepoOverrides(&locked, override)
		if !errors.Is(err, config.ErrCustomPromptsDisallowed) {
			t.Fatalf("expected ErrCustomPromptsDisallowed, got %v", err)
		}
	})

	t.Run("rejects custom prompt outside .mochi directory or .mochi directory itself", func(t *testing.T) {
		for _, p := range []string{"configs/prompts/security.md", ".mochi", ".mochi/", "./.mochi"} {
			override := []byte("agents:\n  - role: correctness\n    prompt_file: \"" + p + "\"\n")
			if _, err := config.MergeRepoOverrides(wf, override); err == nil {
				t.Fatalf("expected error for repo prompt %q, got nil", p)
			}
		}
	})

	t.Run("rejects path traversal in custom prompt", func(t *testing.T) {
		override := []byte(`
agents:
  - role: correctness
    prompt_file: "../../etc/passwd"
`)
		if _, err := config.MergeRepoOverrides(wf, override); err == nil {
			t.Fatal("expected error for escaping prompt path, got nil")
		}
	})

	t.Run("rejects llm base_url provider and model overrides", func(t *testing.T) {
		for _, snippet := range []string{
			"base_url: \"https://evil.example/v1\"",
			"provider: \"openai\"",
			"model: \"gpt-evil\"",
		} {
			override := []byte("agents:\n  - role: correctness\n    llm:\n      " + snippet + "\n")
			_, err := config.MergeRepoOverrides(wf, override)
			if !errors.Is(err, config.ErrDisallowedOverrideField) {
				t.Fatalf("snippet %q: expected ErrDisallowedOverrideField, got %v", snippet, err)
			}
		}
	})

	t.Run("rejects tool escalation beyond base role allowlist", func(t *testing.T) {
		override := []byte(`
agents:
  - role: correctness
    tools: ["read_file", "write_file"]
`)
		_, err := config.MergeRepoOverrides(wf, override)
		if !errors.Is(err, config.ErrDisallowedToolOverride) {
			t.Fatalf("expected ErrDisallowedToolOverride, got %v", err)
		}
	})

	t.Run("rejects adding new webhook sink or external url or verdict_mode override", func(t *testing.T) {
		for _, override := range []string{
			"output:\n  sinks:\n    - type: webhook\n      url: \"https://evil.example/hook\"\n",
			"output:\n  sinks:\n    - type: github_pr_review\n      url: \"https://evil.example/hook\"\n",
			"output:\n  sinks:\n    - type: github_pr_review\n      verdict_mode: \"enforce\"\n",
		} {
			_, err := config.MergeRepoOverrides(wf, []byte(override))
			if !errors.Is(err, config.ErrDisallowedOverrideField) {
				t.Fatalf("override %q: expected ErrDisallowedOverrideField, got %v", override, err)
			}
		}
	})

	t.Run("rejects disabling all specialist agents via post-merge validation", func(t *testing.T) {
		override := []byte(`
agents:
  - role: correctness
    enabled: false
  - role: security
    enabled: false
  - role: performance-architecture
    enabled: false
  - role: maintainability
    enabled: false
  - role: test-runner
    enabled: false
`)
		if _, err := config.MergeRepoOverrides(wf, override); err == nil {
			t.Fatal("expected validation error when all agents are disabled, got nil")
		}
	})

	t.Run("rejects input.triggers override because webhooks run before repo override load", func(t *testing.T) {
		override := []byte(`
input:
  triggers:
    events: ["opened"]
    comment_command: "/review"
`)
		_, err := config.MergeRepoOverrides(wf, override)
		if !errors.Is(err, config.ErrDisallowedOverrideField) {
			t.Fatalf("expected ErrDisallowedOverrideField for input.triggers, got %v", err)
		}
	})

	t.Run("clamps unset budgets and persists prompt_from_repo", func(t *testing.T) {
		// Exercise unset-budget ceilings independently of the deployment preset's caps.
		base := *wf
		base.Agents = append([]domain.RoleConfig(nil), wf.Agents...)
		base.Agents[4].LLM.MaxOutputTokens = 0
		base.Synthesizer.Budgets.MaxTokens = 0
		override := []byte(`
agents:
  - role: correctness
    prompt_file: ".mochi/prompts/correctness.md"
    llm:
      reasoning_effort: low
    tools: ["read_file", "submit_output"]
    budgets:
      max_turns: 999
      run_timeout: 2m
  - role: test-runner
    llm:
      max_output_tokens: 999999
      reasoning_effort: high
  - role: performance-architecture
    enabled: false
  - role: maintainability
    enabled: false
synthesizer:
  prompt_file: ".mochi/prompts/synth.md"
  budgets:
    max_tokens: 99999999
output:
  sinks:
    - type: github_pr_review
      min_severity: "medium"
`)
		merged, err := config.MergeRepoOverrides(&base, override)
		if err != nil {
			t.Fatalf("MergeRepoOverrides failed: %v", err)
		}
		if !merged.Input.Triggers.IgnoreDrafts {
			t.Fatal("expected ignore_drafts=true to be preserved")
		}
		if len(merged.Agents) != 3 {
			t.Fatalf("expected 3 agents after disabling performance-architecture and maintainability, got %d", len(merged.Agents))
		}
		if merged.Agents[0].Budgets.MaxTurns != 15 {
			t.Fatalf("expected max_turns clamped to base 15, got %d", merged.Agents[0].Budgets.MaxTurns)
		}
		if merged.Agents[0].Budgets.RunTimeout.Std() != 2*time.Minute {
			t.Fatalf("expected run_timeout lowered to 2m, got %v", merged.Agents[0].Budgets.RunTimeout.Std())
		}
		if merged.Agents[0].LLM.ReasoningEffort != "low" {
			t.Fatalf("expected correctness reasoning_effort lowered to low, got %q", merged.Agents[0].LLM.ReasoningEffort)
		}
		if !merged.Agents[0].PromptFromRepo || merged.Agents[0].PromptFile != ".mochi/prompts/correctness.md" {
			t.Fatalf("unexpected custom prompt metadata: %+v", merged.Agents[0])
		}
		if !merged.Synthesizer.PromptFromRepo || merged.Synthesizer.PromptFile != ".mochi/prompts/synth.md" {
			t.Fatalf("unexpected synthesizer custom prompt metadata: %+v", merged.Synthesizer)
		}
		// test-runner is at index 2 after performance-architecture is disabled.
		if merged.Agents[2].LLM.MaxOutputTokens != 16384 {
			t.Fatalf("expected unset max_output_tokens clamped to ceiling 16384, got %d", merged.Agents[2].LLM.MaxOutputTokens)
		}
		if merged.Agents[2].LLM.ReasoningEffort != "high" {
			t.Fatalf("expected test-runner reasoning_effort raised to high, got %q", merged.Agents[2].LLM.ReasoningEffort)
		}
		if merged.Synthesizer.Budgets.MaxTokens != 200000 {
			t.Fatalf("expected unset synthesizer max_tokens clamped to ceiling 200000, got %d", merged.Synthesizer.Budgets.MaxTokens)
		}
		if len(merged.Agents[0].Tools) != 2 {
			t.Fatalf("expected subset tools, got %v", merged.Agents[0].Tools)
		}
		if merged.Output.Sinks[0].MinSeverity != "medium" {
			t.Fatalf("expected min_severity=medium, got %q", merged.Output.Sinks[0].MinSeverity)
		}

		// Verify PromptFromRepo survives MarshalWorkflowYAML -> ParseResolvedWorkflow, while ParseWorkflow rejects it.
		resolvedYAML, err := config.MarshalWorkflowYAML(merged)
		if err != nil {
			t.Fatalf("MarshalWorkflowYAML failed: %v", err)
		}
		if !strings.Contains(resolvedYAML, "prompt_from_repo: true") {
			t.Fatalf("expected prompt_from_repo in resolved YAML:\n%s", resolvedYAML)
		}
		reloaded, err := config.ParseResolvedWorkflow([]byte(resolvedYAML))
		if err != nil {
			t.Fatalf("ParseResolvedWorkflow failed: %v", err)
		}
		if !reloaded.Agents[0].PromptFromRepo || !reloaded.Synthesizer.PromptFromRepo {
			t.Fatalf("PromptFromRepo lost across resolved YAML round-trip: agent=%v synth=%v", reloaded.Agents[0].PromptFromRepo, reloaded.Synthesizer.PromptFromRepo)
		}
		if _, err := config.ParseWorkflow([]byte(resolvedYAML)); err == nil {
			t.Fatal("expected ParseWorkflow to reject prompt_from_repo=true in server workflow YAML")
		}

		if wf.Agents[0].Budgets.MaxTurns != 15 || len(wf.Agents) != 5 || wf.Agents[0].PromptFromRepo {
			t.Fatal("base workflow was mutated by MergeRepoOverrides")
		}
	})
}

func TestPromptEngineSecurityAndRendering(t *testing.T) {
	root := repoRoot(t)
	wf, err := config.LoadWorkflowFile(filepath.Join(root, "configs", "workflows", "pr-review.yaml"), root)
	if err != nil {
		t.Fatalf("LoadWorkflowFile failed: %v", err)
	}

	repoCheckout := t.TempDir()
	mochiDir := filepath.Join(repoCheckout, ".mochi", "prompts")
	if err := os.MkdirAll(mochiDir, 0o755); err != nil {
		t.Fatalf("mkdir repo .mochi: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mochiDir, "custom.md"), []byte("Custom prompt for {{ .Task.Changeset.RepoName }} ({{ .Task.Workspace.RepoURL }})"), 0o644); err != nil {
		t.Fatalf("write repo prompt: %v", err)
	}

	// Create a symlink inside .mochi/prompts attempting to escape repoCheckout.
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("TOP_SECRET"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(mochiDir, "escape.md")); err != nil {
		t.Fatalf("symlink failed: %v", err)
	}

	engine := config.NewPromptEngineWithRoots(root, repoCheckout)
	task := &domain.TaskContext{
		RunID:        "run-42",
		WorkflowName: wf.Name,
		TriggerType:  "github_pr",
		Workspace: domain.WorkspaceSpec{
			RepoURL:    "https://x-access-token:ghs_URL_SECRET@github.com/shohei-yamagiwa/mochi.git",
			CloneToken: "ghs_SECRET_DO_NOT_LEAK",
		},
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			HeadSHA:   "1234567890abcdef",
		},
	}

	for i := range wf.Agents {
		role := wf.Agents[i]
		data := config.NewPromptData(task, wf, &role)
		rendered, err := engine.RenderRole(role, data)
		if err != nil {
			t.Fatalf("RenderRole(%s) failed: %v", role.Role, err)
		}
		if !strings.Contains(rendered, "shohei-yamagiwa/mochi") || !strings.Contains(rendered, "1234567") {
			t.Fatalf("rendered prompt for %s missing expected context:\n%s", role.Role, rendered)
		}
	}

	synthRendered, err := engine.RenderSynthesizer(wf.Synthesizer, config.NewPromptData(task, wf, nil))
	if err != nil {
		t.Fatalf("RenderSynthesizer failed: %v", err)
	}
	if !strings.Contains(synthRendered, "1234567") {
		t.Fatalf("rendered synthesizer prompt missing shortSHA:\n%s", synthRendered)
	}

	// Verify all default templates also render cleanly when TaskContext.Changeset is nil.
	nilChangesetTask := &domain.TaskContext{
		RunID:        "run-no-changeset",
		WorkflowName: wf.Name,
		TriggerType:  "cli",
	}
	for i := range wf.Agents {
		role := wf.Agents[i]
		if _, err := engine.RenderRole(role, config.NewPromptData(nilChangesetTask, wf, &role)); err != nil {
			t.Fatalf("RenderRole(%s) with nil Changeset failed: %v", role.Role, err)
		}
	}
	if _, err := engine.RenderSynthesizer(wf.Synthesizer, config.NewPromptData(nilChangesetTask, wf, nil)); err != nil {
		t.Fatalf("RenderSynthesizer with nil Changeset failed: %v", err)
	}

	// RenderRole with PromptFromRepo=true automatically selects repoDir and strips RepoURL userinfo.
	repoRole := wf.Agents[0]
	repoRole.PromptFile = ".mochi/prompts/custom.md"
	repoRole.PromptFromRepo = true
	customOut, err := engine.RenderRole(repoRole, config.NewPromptData(task, wf, &repoRole))
	if err != nil {
		t.Fatalf("RenderRole(repo) failed: %v", err)
	}
	if strings.Contains(customOut, "ghs_URL_SECRET") || strings.Contains(customOut, "x-access-token") {
		t.Fatalf("RepoURL userinfo leaked into rendered prompt: %q", customOut)
	}
	if customOut != "Custom prompt for mochi (https://github.com/shohei-yamagiwa/mochi.git)" {
		t.Fatalf("unexpected custom prompt output: %q", customOut)
	}

	// Escaping symlink under .mochi/ is blocked by os.Root.
	if _, err := engine.RenderRepoFile(".mochi/prompts/escape.md", config.NewPromptData(task, wf, nil)); err == nil {
		t.Fatal("expected symlink escape to be rejected by os.Root, got nil")
	}

	// CloneToken cannot be evaluated in templates.
	if _, err := engine.RenderTemplate("leak", "{{ .Task.Workspace.CloneToken }}", config.NewPromptData(task, wf, nil)); err == nil {
		t.Fatal("expected error referencing CloneToken from PromptData, got nil")
	}

	// UTF-8 rune-safe truncation.
	utf8Out, err := engine.RenderTemplate("utf8", `{{ truncate 2 "あいう" }}`, config.NewPromptData(task, wf, nil))
	if err != nil {
		t.Fatalf("RenderTemplate(utf8) failed: %v", err)
	}
	if utf8Out != "あい...(truncated)" {
		t.Fatalf("got %q, want %q", utf8Out, "あい...(truncated)")
	}

	if _, err := engine.RenderTemplate("strict", "hello {{ .MissingField }}", config.NewPromptData(task, wf, nil)); err == nil {
		t.Fatal("expected error for missing template field")
	}

	// Operator sink secrets (URL and Options) must not be readable from PromptData.Workflow.
	wfWithSecrets := *wf
	wfWithSecrets.Output.Sinks = []domain.SinkConfig{
		{
			Type: "webhook",
			URL:  "https://hooks.internal.example/secret-token-123",
			Options: map[string]any{
				"secret":       "hmac-signing-secret-xyz",
				"bearer_token": "tok-operator-999",
			},
		},
	}
	secretData := config.NewPromptData(task, &wfWithSecrets, nil)
	if _, err := engine.RenderTemplate("sink_url", "{{ (index .Workflow.Output.Sinks 0).URL }}", secretData); err == nil {
		t.Fatal("expected error accessing sink URL from PromptData.Workflow")
	}
	if _, err := engine.RenderTemplate("sink_opts", "{{ (index .Workflow.Output.Sinks 0).Options }}", secretData); err == nil {
		t.Fatal("expected error accessing sink Options from PromptData.Workflow")
	}
	outJSON, err := engine.RenderTemplate("sink_json", "{{ toJSON .Workflow }}", secretData)
	if err != nil {
		t.Fatalf("toJSON .Workflow failed: %v", err)
	}
	for _, secret := range []string{"secret-token-123", "hmac-signing-secret-xyz", "tok-operator-999"} {
		if strings.Contains(outJSON, secret) {
			t.Fatalf("sink secret %q leaked via toJSON .Workflow:\n%s", secret, outJSON)
		}
	}
}

func TestReasoningEffortValidation(t *testing.T) {
	root := repoRoot(t)
	wf, err := config.LoadWorkflowFile(filepath.Join(root, "configs", "workflows", "pr-review.yaml"), root)
	if err != nil {
		t.Fatalf("LoadWorkflowFile: %v", err)
	}

	for _, valid := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		override := []byte("agents:\n  - role: correctness\n    llm:\n      reasoning_effort: " + valid + "\n")
		merged, err := config.MergeRepoOverrides(wf, override)
		if err != nil {
			t.Fatalf("expected reasoning_effort %q to be valid, got error: %v", valid, err)
		}
		if merged.Agents[0].LLM.ReasoningEffort != valid {
			t.Fatalf("got ReasoningEffort = %q, want %q", merged.Agents[0].LLM.ReasoningEffort, valid)
		}
		resolvedYAML, err := config.MarshalWorkflowYAML(merged)
		if err != nil {
			t.Fatalf("MarshalWorkflowYAML(%q): %v", valid, err)
		}
		if _, err := config.ParseWorkflow([]byte(resolvedYAML)); err != nil {
			t.Fatalf("ParseWorkflow(%q): %v", valid, err)
		}
	}

	for _, invalid := range []string{"none", "ultra"} {
		override := []byte("agents:\n  - role: correctness\n    llm:\n      reasoning_effort: " + invalid + "\n")
		if _, err := config.MergeRepoOverrides(wf, override); err == nil {
			t.Fatalf("expected reasoning_effort %q to be rejected by MergeRepoOverrides", invalid)
		}
	}
}
