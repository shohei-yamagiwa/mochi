package config_test

import (
	"errors"
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

	t.Run("rejects path traversal in custom prompt", func(t *testing.T) {
		override := []byte(`
agents:
  - role: correctness
    prompt_file: "../../etc/passwd"
`)
		_, err := config.MergeRepoOverrides(wf, override)
		if err == nil {
			t.Fatal("expected error for escaping prompt path, got nil")
		}
	})

	t.Run("merges allowed overrides without mutating base", func(t *testing.T) {
		override := []byte(`
agents:
  - role: correctness
    budgets:
      max_turns: 8
  - role: performance-architecture
    enabled: false
`)
		merged, err := config.MergeRepoOverrides(wf, override)
		if err != nil {
			t.Fatalf("MergeRepoOverrides failed: %v", err)
		}
		if len(merged.Agents) != 3 {
			t.Fatalf("expected 3 agents after disabling performance-architecture, got %d", len(merged.Agents))
		}
		if merged.Agents[0].Budgets.MaxTurns != 8 {
			t.Fatalf("expected correctness max_turns=8, got %d", merged.Agents[0].Budgets.MaxTurns)
		}
		if wf.Agents[0].Budgets.MaxTurns != 15 || len(wf.Agents) != 4 {
			t.Fatal("base workflow was mutated by MergeRepoOverrides")
		}
	})
}

func TestPromptEngineRenderDefaultTemplates(t *testing.T) {
	root := repoRoot(t)
	wf, err := config.LoadWorkflowFile(filepath.Join(root, "configs", "workflows", "pr-review.yaml"), root)
	if err != nil {
		t.Fatalf("LoadWorkflowFile failed: %v", err)
	}

	engine := config.NewPromptEngine(root)
	task := &domain.TaskContext{
		RunID:        "run-42",
		WorkflowName: wf.Name,
		TriggerType:  "github_pr",
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			HeadSHA:   "1234567890abcdef",
		},
	}

	for i := range wf.Agents {
		role := wf.Agents[i]
		rendered, err := engine.RenderFile(role.PromptFile, config.PromptData{
			Task:     task,
			Workflow: wf,
			Role:     &role,
		})
		if err != nil {
			t.Fatalf("RenderFile(%s) failed: %v", role.PromptFile, err)
		}
		if !strings.Contains(rendered, "shohei-yamagiwa/mochi") || !strings.Contains(rendered, "1234567") {
			t.Fatalf("rendered prompt for %s missing expected context:\n%s", role.Role, rendered)
		}
	}

	synthRendered, err := engine.RenderFile(wf.Synthesizer.PromptFile, config.PromptData{
		Task:     task,
		Workflow: wf,
	})
	if err != nil {
		t.Fatalf("RenderFile(synthesizer) failed: %v", err)
	}
	if !strings.Contains(synthRendered, "1234567") {
		t.Fatalf("rendered synthesizer prompt missing shortSHA:\n%s", synthRendered)
	}

	if _, err := engine.RenderTemplate("strict", "hello {{ .MissingField }}", config.PromptData{}); err == nil {
		t.Fatal("expected error for missing template field")
	}
}
