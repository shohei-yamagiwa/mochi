package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/store"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
)

func TestSQLiteStoreFullLifecycle(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mochi-audit.db")

	st, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Millisecond)
	run := &domain.RunRecord{
		RunID:              "run-100",
		WorkflowName:       "pr-review",
		TriggerType:        "github_pr",
		DedupKey:           "github:shohei-yamagiwa/mochi:pr:7",
		Status:             domain.RunStatusRunning,
		ResolvedConfigYAML: "name: pr-review\n",
		StartedAt:          now,
	}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	cs := &domain.ChangesetSpec{
		Provider:     "github",
		RepoOwner:    "shohei-yamagiwa",
		RepoName:     "mochi",
		PRNumber:     7,
		PRURL:        "https://github.com/shohei-yamagiwa/mochi/pull/7",
		Title:        "Add phase 1 foundation",
		Author:       "shohei-yamagiwa",
		TriggerEvent: "pull_request.opened",
		BaseBranch:   "develop",
		BaseSHA:      "aaaa1111",
		HeadBranch:   "feat/phase-1",
		HeadSHA:      "bbbb2222",
		UnifiedDiff:  "diff --git a/main.go b/main.go\n",
		ChangedFiles: []domain.ChangedFile{
			{
				Path:      "main.go",
				Status:    "modified",
				Additions: 5,
				Deletions: 1,
				Patch:     "@@ -1,2 +1,6 @@",
				ChangedHunks: []domain.LineRange{
					{StartLine: 1, EndLine: 6},
				},
			},
		},
	}
	if err := st.SaveChangesetSnapshot(ctx, run.RunID, cs); err != nil {
		t.Fatalf("SaveChangesetSnapshot failed: %v", err)
	}

	ar := &domain.AgentRunRecord{
		AgentRunID:           "agent-100-correctness",
		RunID:                run.RunID,
		Role:                 "correctness",
		Provider:             "anthropic",
		Model:                "claude-sonnet-5-5",
		SandboxID:            "sbx-1",
		RenderedSystemPrompt: "You are correctness.",
		RenderedUserPrompt:   "Review PR #7.",
		Status:               domain.AgentRunStatusRunning,
		StartedAt:            now,
	}
	if err := st.CreateAgentRun(ctx, ar); err != nil {
		t.Fatalf("CreateAgentRun failed: %v", err)
	}

	// Verify FK enforcement via DSN pragma.
	orphanAgent := &domain.AgentRunRecord{
		AgentRunID: "agent-orphan",
		RunID:      "nonexistent-run",
		Role:       "security",
		Provider:   "anthropic",
		Model:      "claude-opus-5-5",
		Status:     domain.AgentRunStatusRunning,
		StartedAt:  now,
	}
	if err := st.CreateAgentRun(ctx, orphanAgent); err == nil {
		t.Fatal("expected foreign key error for nonexistent run_id, got nil")
	}

	turn := &domain.LLMTurnRecord{
		TurnID:              "turn-1",
		AgentRunID:          ar.AgentRunID,
		TurnIndex:           0,
		RequestMessagesJSON: json.RawMessage(`[{"role":"user","content":"Review PR #7."}]`),
		ResponseMessageJSON: json.RawMessage(`{"role":"assistant","content":"Let me inspect main.go."}`),
		ThinkingTrace:       "Inspecting main.go first",
		ToolCallsJSON:       json.RawMessage(`[{"id":"tc-1","name":"read_file","arguments":{"path":"main.go","start_line":1,"end_line":20}}]`),
		InputTokens:         500,
		OutputTokens:        120,
		CachedTokens:        200,
		LatencyMS:           450,
		CreatedAt:           now.Add(time.Second),
	}
	if err := st.SaveLLMTurn(ctx, turn); err != nil {
		t.Fatalf("SaveLLMTurn failed: %v", err)
	}

	execRec := &domain.ToolExecRecord{
		ToolExecID:    "texec-1",
		AgentRunID:    ar.AgentRunID,
		TurnID:        turn.TurnID,
		ToolCallID:    "tc-1",
		ToolName:      "read_file",
		ArgumentsJSON: json.RawMessage(`{"path":"main.go","start_line":1,"end_line":20}`),
		OutputText:    "package main\n",
		ExitCode:      0,
		AccessedFiles: []domain.FileAccessRecord{
			{
				AgentRole: "correctness",
				Path:      "main.go",
				Kind:      domain.FileAccessRead,
				LineRange: &domain.LineRange{StartLine: 1, EndLine: 20},
			},
		},
		DurationMS: 35,
		CreatedAt:  now.Add(2 * time.Second),
	}
	if err := st.SaveToolExecution(ctx, execRec); err != nil {
		t.Fatalf("SaveToolExecution failed: %v", err)
	}

	completedAt := now.Add(5 * time.Second)
	ar.Status = domain.AgentRunStatusCompleted
	ar.TurnsUsed = 1
	ar.InputTokens = 500
	ar.OutputTokens = 120
	ar.CachedTokens = 200
	ar.SubmittedOutputJSON = json.RawMessage(`{"findings":[]}`)
	ar.CompletedAt = &completedAt
	if err := st.UpdateAgentRun(ctx, ar); err != nil {
		t.Fatalf("UpdateAgentRun failed: %v", err)
	}

	// Updating with nil CompletedAt should preserve existing completed_at (COALESCE semantics).
	ar.CompletedAt = nil
	if err := st.UpdateAgentRun(ctx, ar); err != nil {
		t.Fatalf("UpdateAgentRun(nil CompletedAt) failed: %v", err)
	}

	cov := &domain.CoverageReport{
		RunID:                run.RunID,
		PRNumber:             7,
		BaseSHA:              "aaaa1111",
		HeadSHA:              "bbbb2222",
		TotalChangedFiles:    1,
		ReviewedChangedFiles: 1,
		FileCoverageRatio:    1.0,
		TotalChangedHunks:    1,
		ReviewedChangedHunks: 1,
		HunkCoverageRatio:    1.0,
		UnreviewedFiles:      []string{},
		FileDetails: []domain.FileCoverageDetail{
			{
				Path:              "main.go",
				ChangeStatus:      "modified",
				ChangedHunks:      []domain.LineRange{{StartLine: 1, EndLine: 6}},
				ReadRangesByAgent: map[string][]domain.LineRange{"correctness": {{StartLine: 1, EndLine: 20}}},
				InspectedByAgents: []string{"correctness"},
				UncoveredHunks:    []domain.LineRange{},
				FindingsCount:     0,
			},
		},
		ContextFilesRead: []string{"go.mod"},
		CreatedAt:        completedAt,
	}
	if err := st.SaveCoverageReport(ctx, cov); err != nil {
		t.Fatalf("SaveCoverageReport failed: %v", err)
	}

	finalArtifact := json.RawMessage(`{"summary":"Looks good","verdict":"APPROVE","findings":[]}`)
	if err := st.CompleteRun(ctx, run.RunID, domain.RunStatusCompleted, finalArtifact, completedAt); err != nil {
		t.Fatalf("CompleteRun failed: %v", err)
	}

	detail, err := st.GetRunDetail(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRunDetail failed: %v", err)
	}
	if detail.Run.Status != domain.RunStatusCompleted {
		t.Fatalf("got status %q, want completed", detail.Run.Status)
	}
	if detail.Changeset == nil || detail.Changeset.HeadSHA != "bbbb2222" || len(detail.Changeset.ChangedFiles) != 1 {
		t.Fatalf("unexpected changeset in RunDetail: %+v", detail.Changeset)
	}
	if len(detail.AgentRuns) != 1 || detail.AgentRuns[0].AgentRun.CompletedAt == nil {
		t.Fatalf("expected agent_run completed_at preserved, got %+v", detail.AgentRuns)
	}
	if len(detail.AgentRuns[0].LLMTurns) != 1 || len(detail.AgentRuns[0].ToolExecutions) != 1 {
		t.Fatalf("unexpected agent run detail counts: %+v", detail.AgentRuns)
	}
	if detail.AgentRuns[0].ToolExecutions[0].AccessedFiles[0].Path != "main.go" {
		t.Fatalf("unexpected accessed files: %+v", detail.AgentRuns[0].ToolExecutions[0].AccessedFiles)
	}
	if detail.Coverage == nil || detail.Coverage.FileCoverageRatio != 1.0 {
		t.Fatalf("unexpected coverage report: %+v", detail.Coverage)
	}

	byRunExecs, err := st.ListToolExecutionsByRun(ctx, run.RunID)
	if err != nil || len(byRunExecs) != 1 {
		t.Fatalf("ListToolExecutionsByRun failed: len=%d err=%v", len(byRunExecs), err)
	}

	runs, err := st.ListRuns(ctx, store.RunFilter{DedupKey: run.DedupKey, Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns failed: len=%d err=%v", len(runs), err)
	}

	if _, err := st.GetRunDetail(ctx, "does-not-exist"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing run, got %v", err)
	}
}

func TestSQLiteStoreSubSecondTimestampOrdering(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open(:memory:) failed: %v", err)
	}
	defer st.Close()

	// Exact whole-second timestamp vs +500ms timestamp within the same second.
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(500 * time.Millisecond)

	if err := st.CreateRun(ctx, &domain.RunRecord{
		RunID:        "run-older",
		WorkflowName: "pr-review",
		TriggerType:  "cli",
		DedupKey:     "k1",
		Status:       domain.RunStatusCompleted,
		StartedAt:    t0,
	}); err != nil {
		t.Fatalf("CreateRun(older) failed: %v", err)
	}
	if err := st.CreateRun(ctx, &domain.RunRecord{
		RunID:        "run-newer",
		WorkflowName: "pr-review",
		TriggerType:  "cli",
		DedupKey:     "k2",
		Status:       domain.RunStatusCompleted,
		StartedAt:    t1,
	}); err != nil {
		t.Fatalf("CreateRun(newer) failed: %v", err)
	}

	runs, err := st.ListRuns(ctx, store.RunFilter{})
	if err != nil {
		t.Fatalf("ListRuns failed: %v", err)
	}
	if len(runs) != 2 || runs[0].RunID != "run-newer" || runs[1].RunID != "run-older" {
		t.Fatalf("ListRuns sub-second ordering wrong: got [%s, %s], want [run-newer, run-older]", runs[0].RunID, runs[1].RunID)
	}

	if err := st.CreateAgentRun(ctx, &domain.AgentRunRecord{
		AgentRunID: "ar-1",
		RunID:      "run-older",
		Role:       "correctness",
		Provider:   "anthropic",
		Model:      "claude-sonnet-5-5",
		Status:     domain.AgentRunStatusRunning,
		StartedAt:  t0,
	}); err != nil {
		t.Fatalf("CreateAgentRun failed: %v", err)
	}

	// Save newer before older? No, save older (t0) and newer (t1), and verify ASC ordering.
	if err := st.SaveToolExecution(ctx, &domain.ToolExecRecord{
		ToolExecID: "exec-2",
		AgentRunID: "ar-1",
		ToolCallID: "tc-2",
		ToolName:   "read_file",
		CreatedAt:  t1,
	}); err != nil {
		t.Fatalf("SaveToolExecution(t1) failed: %v", err)
	}
	if err := st.SaveToolExecution(ctx, &domain.ToolExecRecord{
		ToolExecID: "exec-1",
		AgentRunID: "ar-1",
		ToolCallID: "tc-1",
		ToolName:   "read_file",
		CreatedAt:  t0,
	}); err != nil {
		t.Fatalf("SaveToolExecution(t0) failed: %v", err)
	}

	execs, err := st.ListToolExecutions(ctx, "ar-1")
	if err != nil {
		t.Fatalf("ListToolExecutions failed: %v", err)
	}
	if len(execs) != 2 || execs[0].ToolExecID != "exec-1" || execs[1].ToolExecID != "exec-2" {
		t.Fatalf("ListToolExecutions sub-second ordering wrong: got [%s, %s], want [exec-1, exec-2]", execs[0].ToolExecID, execs[1].ToolExecID)
	}
}

func TestSQLiteStoreRelativePaths(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	t.Chdir(tmp)

	for _, relPath := range []string{"rel/x.db", "mochi.db"} {
		t.Run(relPath, func(t *testing.T) {
			st, err := sqlite.Open(ctx, relPath)
			if err != nil {
				t.Fatalf("sqlite.Open(%q) failed: %v", relPath, err)
			}
			defer st.Close()

			run := &domain.RunRecord{
				RunID:        "run-rel-" + filepath.Base(relPath),
				WorkflowName: "pr-review",
				TriggerType:  "cli",
				DedupKey:     "rel",
				Status:       domain.RunStatusRunning,
				StartedAt:    time.Now().UTC(),
			}
			if err := st.CreateRun(ctx, run); err != nil {
				t.Fatalf("CreateRun failed: %v", err)
			}
			got, err := st.GetRun(ctx, run.RunID)
			if err != nil {
				t.Fatalf("GetRun failed: %v", err)
			}
			if got.RunID != run.RunID {
				t.Fatalf("got %q, want %q", got.RunID, run.RunID)
			}
		})
	}
}

func TestSQLiteStoreRepoCredentialsCRUD(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	if _, err := st.GetRepoCredential(ctx, "owner", "repo", "gemini"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRepoCredential missing = %v, want ErrNotFound", err)
	}

	if err := st.UpsertRepoCredential(ctx, &domain.RepoCredentialRecord{
		RepoOwner:       "Shohei-Yamagiwa",
		RepoName:        "Mochi",
		Provider:        "GEMINI",
		EncryptedAPIKey: []byte("cipher-1"),
		Nonce:           []byte("nonce-1"),
		BaseURL:         "",
	}); err != nil {
		t.Fatalf("UpsertRepoCredential: %v", err)
	}

	got, err := st.GetRepoCredential(ctx, "shohei-yamagiwa", "mochi", "gemini")
	if err != nil {
		t.Fatalf("GetRepoCredential: %v", err)
	}
	if string(got.EncryptedAPIKey) != "cipher-1" || got.RepoOwner != "shohei-yamagiwa" || got.RepoName != "mochi" || got.Provider != "gemini" {
		t.Fatalf("unexpected credential record: %+v", got)
	}

	// Upsert overwrites encrypted key and base_url.
	if err := st.UpsertRepoCredential(ctx, &domain.RepoCredentialRecord{
		RepoOwner:       "shohei-yamagiwa",
		RepoName:        "mochi",
		Provider:        "gemini",
		EncryptedAPIKey: []byte("cipher-2"),
		Nonce:           []byte("nonce-2"),
		BaseURL:         "https://example.invalid/v1",
	}); err != nil {
		t.Fatalf("UpsertRepoCredential overwrite: %v", err)
	}
	got2, err := st.GetRepoCredential(ctx, "SHOHEI-YAMAGIWA", "MOCHI", "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if string(got2.EncryptedAPIKey) != "cipher-2" || got2.BaseURL != "https://example.invalid/v1" {
		t.Fatalf("unexpected overwritten credential: %+v", got2)
	}

	// List summaries for repo.
	list, err := st.ListRepoCredentials(ctx, "shohei-yamagiwa", "mochi")
	if err != nil {
		t.Fatalf("ListRepoCredentials: %v", err)
	}
	if len(list) != 1 || !list[0].HasAPIKey || list[0].Provider != "gemini" {
		t.Fatalf("unexpected ListRepoCredentials: %+v", list)
	}

	if err := st.DeleteRepoCredential(ctx, "shohei-yamagiwa", "mochi", "gemini"); err != nil {
		t.Fatalf("DeleteRepoCredential: %v", err)
	}
	if err := st.DeleteRepoCredential(ctx, "shohei-yamagiwa", "mochi", "gemini"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second DeleteRepoCredential = %v, want ErrNotFound", err)
	}
}
