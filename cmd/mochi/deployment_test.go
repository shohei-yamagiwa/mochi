package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

func runtimeTestEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"MOCHI_GITHUB_APP_ID", "MOCHI_GITHUB_PRIVATE_KEY", "MOCHI_GITHUB_PRIVATE_KEY_PATH",
		"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_API_URL", "GITHUB_WEBHOOK_SECRET",
		"MOCHI_TRIGGER_SECRET", "MOCHI_API_TOKEN", "MOCHI_MASTER_KEY", "MOCHI_WORKFLOW",
		"DAYTONA_API_KEY", "DAYTONA_JWT_TOKEN",
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENAI_COMPAT_BASE_URL",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("GITHUB_TOKEN", "test-token-no-network")
	t.Setenv("GITHUB_WEBHOOK_SECRET", "test-webhook-secret")
	t.Setenv("MOCHI_API_TOKEN", "test-api-token")
	t.Setenv("MOCHI_MASTER_KEY", "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	t.Setenv("DAYTONA_API_KEY", "test-daytona-key")
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
}

func TestServerRuntimeValidationDoesNotRequireLLMKeysOrWorkflow(t *testing.T) {
	runtimeTestEnv(t)
	// Clear all global LLM API keys and MOCHI_WORKFLOW: server mode must not require them.
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("MOCHI_WORKFLOW", "")

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"validate", "--runtime"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mochi validate --runtime failed without global LLM keys: %v", err)
	}
	if !strings.Contains(out.String(), "valid: server runtime configuration") {
		t.Fatalf("unexpected output: %q", out.String())
	}

	for _, tc := range []struct{ key, want string }{
		{"GITHUB_TOKEN", "github authentication"},
		{"GITHUB_WEBHOOK_SECRET", "webhook secret"},
		{"MOCHI_API_TOKEN", "authentication secrets"},
		{"DAYTONA_API_KEY", "DAYTONA_API_KEY"},
		{"MOCHI_MASTER_KEY", "MOCHI_MASTER_KEY"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, "")
			err := validateServerRuntime()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestRuntimeWorkflowChecksAllDependencies(t *testing.T) {
	runtimeTestEnv(t)
	wf := &domain.Workflow{
		Input:       domain.InputConfig{Adapter: "github_pr"},
		Agents:      []domain.RoleConfig{{Role: "correctness", LLM: domain.ModelConfig{Provider: "openai"}, Sandbox: domain.AgentSandboxConfig{Enabled: true}}},
		Synthesizer: domain.SynthesizerConfig{LLM: domain.ModelConfig{Provider: "openai"}},
	}
	if err := validateRuntimeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, want string }{
		{"GITHUB_TOKEN", "github authentication"},
		{"GITHUB_WEBHOOK_SECRET", "webhook secret"},
		{"MOCHI_API_TOKEN", "authentication secrets"},
		{"DAYTONA_API_KEY", "DAYTONA_API_KEY"},
		{"OPENAI_API_KEY", "OPENAI_API_KEY"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, "")
			err := validateRuntimeWorkflow(wf)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error mentioning %q", err, tc.want)
			}
		})
	}
	wf.Synthesizer.LLM.Provider = "anthropic"
	if err := validateRuntimeWorkflow(wf); err == nil || !strings.Contains(err.Error(), "synthesizer") {
		t.Fatalf("missing synthesizer credential not detected: %v", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	if err := validateRuntimeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	wf.Synthesizer.LLM.Provider = "gemini"
	t.Setenv("GOOGLE_API_KEY", "test-google-key")
	if err := validateRuntimeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	wf.Synthesizer.LLM.Provider = "openai_compat"
	t.Setenv("OPENAI_COMPAT_BASE_URL", "http://localhost:11434/v1")
	if err := validateRuntimeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOCHI_GITHUB_APP_ID", "123")
	t.Setenv("MOCHI_GITHUB_PRIVATE_KEY", "invalid-pem")
	if err := validateRuntimeWorkflow(wf); err == nil {
		t.Fatal("malformed GitHub App key accepted")
	}
}

func TestCLI_ValidateDeploymentPresets(t *testing.T) {
	for _, tc := range []struct{ file, expected string }{
		{"pr-review-small.yaml", "1 agents, 1 sinks"},
		{"pr-review.yaml", "5 agents, 1 sinks"},
	} {
		t.Run(tc.file, func(t *testing.T) { testValidateDeploymentPreset(t, tc.file, tc.expected) })
	}
}

func testValidateDeploymentPreset(t *testing.T, file, expected string) {
	t.Helper()
	runtimeTestEnv(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "test-gemini-key")
	path := filepath.Join("..", "..", ".mochi", "workflows", file)
	for _, runtime := range []bool{false, true} {
		cmd := newRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		args := []string{"validate", path}
		if runtime {
			args = append(args, "--runtime")
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("unexpected output: %s", out.String())
		}
	}
	// Plain validation remains usable in CI without deployment secrets.
	t.Setenv("GEMINI_API_KEY", "")
	cmd := newRootCmd()
	cmd.SetOut(os.Stdout)
	cmd.SetArgs([]string{"validate", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	cmd = newRootCmd()
	cmd.SetArgs([]string{"validate", path, "--runtime"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("missing Gemini key was not reported: %v", err)
	}
}
