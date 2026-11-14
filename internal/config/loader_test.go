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
	if len(wf.Agents) != 4 {
		t.Fatalf("got %d agents, want 4", len(wf.Agents))
	}
	if wf.Agents[3].Role != "test-runner" || wf.Agents[3].Budgets.RunTimeout.Std() != 8*time.Minute {
		t.Fatalf("unexpected test-runner config: %+v", wf.Agents[3])
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
		override := []byte(`
sandbox:
  default_image: "evil/exfiltrate:latest"
`)
		_, err := config.MergeRepoOverrides(wf, override)
		if !errors.Is(err, config.ErrCustomImagesDisallowed) {
			t.Fatalf("expected ErrCustomImagesDisallowed, got %v", err)
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

	t.Run("rejects custom prompt outside .mochi directory", func(t *testing.T) {
		override := []byte(`
agents:
  - role: correctness
    prompt_file: "configs/prompts/security.md"
`)
		if _, err := config.MergeRepoOverrides(wf, override); err == nil {
			t.Fatal("expected error for repo prompt outside .mochi/, got nil")
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
  - role: test-runner
    enabled: false
`)
		if _, err := config.MergeRepoOverrides(wf, override); err == nil {
			t.Fatal("expected validation error when all agents are disabled, got nil")
		}
	})

	t.Run("clamps budgets subsets tools preserves ignore_drafts and does not mutate base", func(t *testing.T) {
		override := []byte(`
input:
  triggers:
    events: ["opened"]
agents:
  - role: correctness
    prompt_file: ".mochi/prompts/correctness.md"
    tools: ["read_file", "submit_output"]
    budgets:
      max_turns: 999
      run_timeout: 2m
  - role: performance-architecture
    enabled: false
output:
  sinks:
    - type: github_pr_review
      min_severity: "medium"
`)
		merged, err := config.MergeRepoOverrides(wf, override)
		if err != nil {
			t.Fatalf("MergeRepoOverrides failed: %v", err)
		}
		if !merged.Input.Triggers.IgnoreDrafts {
			t.Fatal("expected ignore_drafts=true to be preserved when unset in override")
		}
		if len(merged.Agents) != 3 {
			t.Fatalf("expected 3 agents after disabling performance-architecture, got %d", len(merged.Agents))
		}
		if merged.Agents[0].Budgets.MaxTurns != 15 {
			t.Fatalf("expected max_turns clamped to base 15, got %d", merged.Agents[0].Budgets.MaxTurns)
		}
		if merged.Agents[0].Budgets.RunTimeout.Std() != 2*time.Minute {
			t.Fatalf("expected run_timeout lowered to 2m, got %v", merged.Agents[0].Budgets.RunTimeout.Std())
		}
		if !merged.Agents[0].PromptFromRepo || merged.Agents[0].PromptFile != ".mochi/prompts/correctness.md" {
			t.Fatalf("unexpected custom prompt metadata: %+v", merged.Agents[0])
		}
		if len(merged.Agents[0].Tools) != 2 {
			t.Fatalf("expected subset tools, got %v", merged.Agents[0].Tools)
		}
		if merged.Output.Sinks[0].MinSeverity != "medium" {
			t.Fatalf("expected min_severity=medium, got %q", merged.Output.Sinks[0].MinSeverity)
		}
		if wf.Agents[0].Budgets.MaxTurns != 15 || len(wf.Agents) != 4 || wf.Agents[0].PromptFromRepo {
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
	if err := os.WriteFile(filepath.Join(mochiDir, "custom.md"), []byte("Custom prompt for {{ .Task.Changeset.RepoName }}"), 0o644); err != nil {
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
			RepoURL:    "https://github.com/shohei-yamagiwa/mochi.git",
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
		rendered, err := engine.RenderPrompt(role.PromptFile, role.PromptFromRepo, data)
		if err != nil {
			t.Fatalf("RenderPrompt(%s) failed: %v", role.PromptFile, err)
		}
		if !strings.Contains(rendered, "shohei-yamagiwa/mochi") || !strings.Contains(rendered, "1234567") {
			t.Fatalf("rendered prompt for %s missing expected context:\n%s", role.Role, rendered)
		}
	}

	synthRendered, err := engine.RenderPrompt(wf.Synthesizer.PromptFile, wf.Synthesizer.PromptFromRepo, config.NewPromptData(task, wf, nil))
	if err != nil {
		t.Fatalf("RenderPrompt(synthesizer) failed: %v", err)
	}
	if !strings.Contains(synthRendered, "1234567") {
		t.Fatalf("rendered synthesizer prompt missing shortSHA:\n%s", synthRendered)
	}

	// Repo prompt within root renders cleanly.
	customOut, err := engine.RenderRepoFile(".mochi/prompts/custom.md", config.NewPromptData(task, wf, nil))
	if err != nil || customOut != "Custom prompt for mochi" {
		t.Fatalf("RenderRepoFile failed: out=%q err=%v", customOut, err)
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
}
