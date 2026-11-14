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
	if len(detail.AgentRuns) != 1 || len(detail.AgentRuns[0].LLMTurns) != 1 || len(detail.AgentRuns[0].ToolExecutions) != 1 {
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
