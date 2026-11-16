package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/output"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
)

func TestCLI_ValidatePresetWorkflow(t *testing.T) {
	wfPath := filepath.Join("..", "..", "configs", "workflows", "pr-review.yaml")
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"validate", wfPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("mochi validate failed: %v\nstderr: %s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "valid: pr-review") {
		t.Fatalf("unexpected validate output: %q", out.String())
	}
}

func TestCLI_InspectListAndRunDetail(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "inspect.db")
	ctx := context.Background()
	st, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	done := now.Add(4 * time.Second)
	rep := output.ReviewReport{
		Summary:  "Looks good overall.",
		Verdict:  output.VerdictApprove,
		Findings: []output.Finding{},
	}
	repRaw, _ := json.Marshal(rep)

	if err := st.CreateRun(ctx, &domain.RunRecord{
		RunID:        "run-inspect-1",
		WorkflowName: "pr-review",
		TriggerType:  "github_pr",
		DedupKey:     "github:octo/repo:pr:42",
		Status:       domain.RunStatusRunning,
		StartedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveChangesetSnapshot(ctx, "run-inspect-1", &domain.ChangesetSpec{
		Provider:   "github",
		RepoOwner:  "octo",
		RepoName:   "repo",
		PRNumber:   42,
		Title:      "Tighten auth",
		BaseBranch: "main",
		BaseSHA:    "base1111111",
		HeadBranch: "feat/auth",
		HeadSHA:    "head2222222",
		ChangedFiles: []domain.ChangedFile{
			{Path: "auth.go", Status: "modified", ChangedHunks: []domain.LineRange{{StartLine: 1, EndLine: 10}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgentRun(ctx, &domain.AgentRunRecord{
		AgentRunID: "ag-1",
		RunID:      "run-inspect-1",
		Role:       "security",
		Provider:   "anthropic",
		Model:      "claude-opus-5-5",
		Status:     domain.AgentRunStatusCompleted,
		TurnsUsed:  1,
		StartedAt:  now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveLLMTurn(ctx, &domain.LLMTurnRecord{
		TurnID:       "turn-1",
		AgentRunID:   "ag-1",
		TurnIndex:    0,
		InputTokens:  120,
		OutputTokens: 45,
		LatencyMS:    210,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveToolExecution(ctx, &domain.ToolExecRecord{
		ToolExecID: "tool-1",
		AgentRunID: "ag-1",
		TurnID:     "turn-1",
		ToolCallID: "call-1",
		ToolName:   "read_file",
		ExitCode:   0,
		DurationMS: 12,
		AccessedFiles: []domain.FileAccessRecord{
			{AgentRole: "security", Path: "auth.go", Kind: domain.FileAccessRead, LineRange: &domain.LineRange{StartLine: 1, EndLine: 20}},
		},
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCoverageReport(ctx, &domain.CoverageReport{
		RunID:                "run-inspect-1",
		PRNumber:             42,
		BaseSHA:              "base1111111",
		HeadSHA:              "head2222222",
		TotalChangedFiles:    1,
		ReviewedChangedFiles: 1,
		FileCoverageRatio:    1.0,
		TotalChangedHunks:    1,
		ReviewedChangedHunks: 1,
		HunkCoverageRatio:    1.0,
		FileDetails: []domain.FileCoverageDetail{
			{
				Path:              "auth.go",
				ChangeStatus:      "modified",
				ChangedHunks:      []domain.LineRange{{StartLine: 1, EndLine: 10}},
				InspectedByAgents: []string{"security"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteRun(ctx, "run-inspect-1", domain.RunStatusCompleted, repRaw, done); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	// Test run list
	cmd := newRootCmd()
	var listOut bytes.Buffer
	cmd.SetOut(&listOut)
	cmd.SetArgs([]string{"inspect", "--db", dbPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mochi inspect list: %v", err)
	}
	if !strings.Contains(listOut.String(), "run-inspect-1") {
		t.Fatalf("list output missing run ID:\n%s", listOut.String())
	}

	// Test detailed inspect
	cmd = newRootCmd()
	var detailOut bytes.Buffer
	cmd.SetOut(&detailOut)
	cmd.SetArgs([]string{"inspect", "--db", dbPath, "run-inspect-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mochi inspect detail: %v", err)
	}
	for _, want := range []string{
		"Run ID:   run-inspect-1",
		"PR #42 — Tighten auth",
		"security",
		"tool read_file",
		"Coverage Matrix (1/1 files, 100% hunks)",
		"auth.go",
		"Final Report (APPROVE)",
	} {
		if !strings.Contains(detailOut.String(), want) {
			t.Fatalf("inspect output missing %q:\n%s", want, detailOut.String())
		}
	}

	// Test JSON inspect
	cmd = newRootCmd()
	var jsonOut bytes.Buffer
	cmd.SetOut(&jsonOut)
	cmd.SetArgs([]string{"inspect", "--db", dbPath, "--json", "run-inspect-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mochi inspect --json: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid json output: %v\n%s", err, jsonOut.String())
	}
}

func TestCLI_RunDryRunLocalGit(t *testing.T) {
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repoDir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}

	runGit("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")

	runGit("checkout", "-b", "feat/check")
	if err := os.WriteFile(filepath.Join(repoDir, "app.go"), []byte("package app\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "add Hello")

	wfPath := filepath.Join("..", "..", "configs", "workflows", "pr-review.yaml")
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"run", "--dry-run", "--workflow", wfPath, "--repo", repoDir, "--base", "main", "--head", "HEAD"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mochi run --dry-run failed: %v", err)
	}
	for _, want := range []string{
		"Dry Run: workflow=pr-review trigger=cli",
		"app.go",
		"Specialist Agents (5):",
		"correctness",
		"rendered (",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out.String())
		}
	}
}

func TestValidateServeAuthConfig(t *testing.T) {
	if err := validateServeAuthConfig(false, "", "", "", false); err == nil {
		t.Fatalf("expected error when no trigger/API secrets are configured")
	}
	if err := validateServeAuthConfig(true, "", "trig", "api", false); err == nil {
		t.Fatalf("expected error when GitHub is enabled without webhook secret")
	}
	if err := validateServeAuthConfig(true, "hook", "trig", "", false); err != nil {
		t.Fatalf("unexpected error with webhook + trigger secrets: %v", err)
	}
	if err := validateServeAuthConfig(false, "", "", "api", false); err != nil {
		t.Fatalf("unexpected error with API token: %v", err)
	}
	if err := validateServeAuthConfig(true, "", "", "", true); err != nil {
		t.Fatalf("unexpected error with insecureNoAuth=true: %v", err)
	}
}

func TestCLI_ServeRefusesStartWithoutSecrets(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET", "")
	t.Setenv("MOCHI_TRIGGER_SECRET", "")
	t.Setenv("MOCHI_API_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("MOCHI_GITHUB_APP_ID", "")

	wfPath := filepath.Join("..", "..", "configs", "workflows", "pr-review.yaml")
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"serve", "--workflow", wfPath, "--db", filepath.Join(t.TempDir(), "mochi.db")})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected mochi serve to refuse starting without auth secrets")
	}
	if !strings.Contains(err.Error(), "--insecure-no-auth") {
		t.Fatalf("expected error mentioning --insecure-no-auth, got: %v", err)
	}
}
