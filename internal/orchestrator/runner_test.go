package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/config"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/output"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
	"github.com/shohei-yamagiwa/mochi/internal/tools"
)

type trackingSandboxProvider struct {
	mu            sync.Mutex
	createdBase   []*trackingSandbox
	forkedByName  map[string]*trackingSandbox
	files         map[string]string
	searchMatches []sandbox.SearchMatch
}

func newTrackingSandboxProvider(files map[string]string) *trackingSandboxProvider {
	return &trackingSandboxProvider{
		forkedByName: make(map[string]*trackingSandbox),
		files:        files,
	}
}

func (p *trackingSandboxProvider) CreateAndPrepareBaseSandbox(_ context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	copiedFiles := make(map[string]string, len(p.files))
	for k, v := range p.files {
		copiedFiles[k] = v
	}
	sb := &trackingSandbox{
		id:            "sb-base-" + opts.Name,
		name:          opts.Name,
		provider:      p,
		files:         copiedFiles,
		searchMatches: p.searchMatches,
	}
	p.createdBase = append(p.createdBase, sb)
	return sb, nil
}

type trackingSandbox struct {
	mu            sync.Mutex
	id            string
	name          string
	provider      *trackingSandboxProvider
	files         map[string]string
	searchMatches []sandbox.SearchMatch
	destroyed     int
	lastDestroyEr error
}

func (s *trackingSandbox) ID() string { return s.id }

func (s *trackingSandbox) Fork(_ context.Context, name string) (sandbox.Sandbox, error) {
	s.mu.Lock()
	copiedFiles := make(map[string]string, len(s.files))
	for k, v := range s.files {
		copiedFiles[k] = v
	}
	matches := s.searchMatches
	s.mu.Unlock()

	forked := &trackingSandbox{
		id:            "sb-fork-" + name,
		name:          name,
		provider:      s.provider,
		files:         copiedFiles,
		searchMatches: matches,
	}

	s.provider.mu.Lock()
	s.provider.forkedByName[name] = forked
	s.provider.mu.Unlock()

	return forked, nil
}

func (s *trackingSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	return &sandbox.ExecResult{
		ExitCode: 0,
		Stdout:   "PASS: " + req.Command,
	}, nil
}

func (s *trackingSandbox) ReadFile(_ context.Context, path string, _, _ int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if content, ok := s.files[path]; ok {
		return content, nil
	}
	return "package stub\n", nil
}

func (s *trackingSandbox) WriteFile(_ context.Context, path string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = string(content)
	return nil
}

func (s *trackingSandbox) ListDir(_ context.Context, _ string) ([]sandbox.FileEntry, error) {
	return []sandbox.FileEntry{{Name: "service.go", Path: "internal/auth/service.go"}}, nil
}

func (s *trackingSandbox) SearchFiles(_ context.Context, _, _ string) ([]sandbox.SearchMatch, error) {
	if len(s.searchMatches) > 0 {
		return s.searchMatches, nil
	}
	return []sandbox.SearchMatch{
		{
			File:       "internal/auth/service.go",
			LineNumber: 15,
			LineText:   "func VerifyToken(tok string) error {",
		},
	}, nil
}

func (s *trackingSandbox) Destroy(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroyed++
	s.lastDestroyEr = ctx.Err()
	return ctx.Err()
}

type recordingSink struct {
	mu          sync.Mutex
	name        string
	started     int
	published   []json.RawMessage
	failedErrs  []error
	publishErr  error
	startErr    error
}

func (s *recordingSink) Name() string { return s.name }

func (s *recordingSink) OnRunStarted(_ context.Context, _ *domain.TaskContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started++
	return s.startErr
}

func (s *recordingSink) Publish(_ context.Context, _ *domain.TaskContext, artifact json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publishErr != nil {
		return s.publishErr
	}
	cp := make(json.RawMessage, len(artifact))
	copy(cp, artifact)
	s.published = append(s.published, cp)
	return nil
}

func (s *recordingSink) OnRunFailed(_ context.Context, _ *domain.TaskContext, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedErrs = append(s.failedErrs, err)
	return nil
}

type routingLLMProvider struct {
	mu          sync.Mutex
	name        string
	turnByRole  map[string]int
	sysByRole   map[string]string
	handleFn    func(ctx context.Context, role string, turn int, req llm.ChatRequest) (*llm.ChatResponse, error)
}

func newRoutingLLMProvider(name string, fn func(ctx context.Context, role string, turn int, req llm.ChatRequest) (*llm.ChatResponse, error)) *routingLLMProvider {
	return &routingLLMProvider{
		name:       name,
		turnByRole: make(map[string]int),
		sysByRole:  make(map[string]string),
		handleFn:   fn,
	}
}

func (p *routingLLMProvider) Name() string { return p.name }

func (p *routingLLMProvider) Generate(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	role := detectRoleFromRequest(req)
	p.mu.Lock()
	turn := p.turnByRole[role]
	p.turnByRole[role] = turn + 1
	p.sysByRole[role] = req.SystemPrompt
	p.mu.Unlock()
	return p.handleFn(ctx, role, turn, req)
}

func detectRoleFromRequest(req llm.ChatRequest) string {
	if req.ResponseSchema != nil {
		return "synthesizer"
	}
	if len(req.Messages) > 0 {
		first := req.Messages[0].Content
		for _, r := range []string{"correctness", "security", "performance-architecture", "test-runner"} {
			if strings.Contains(first, "## Specialist Role: `"+r+"`") {
				return r
			}
		}
	}
	return "unknown"
}

func TestRunner_EndToEndPresetWorkflowWithParallelFanOutCoverageAndSynthesizerSelfCorrection(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	repoRoot := filepath.Join("..", "..")
	wfPath := filepath.Join(repoRoot, "configs", "workflows", "pr-review.yaml")
	wf, err := config.LoadWorkflowFile(wfPath, repoRoot)
	if err != nil {
		t.Fatalf("LoadWorkflowFile(%q): %v", wfPath, err)
	}

	promptEngine := config.NewPromptEngine(repoRoot)
	sbProvider := newTrackingSandboxProvider(map[string]string{
		"internal/auth/service.go": "package auth\n\nfunc VerifyToken(tok string) error {\n\treturn nil\n}\n",
		"internal/db/store.go":     "package db\n\nfunc Query() {}\n",
	})

	llmProvider := newRoutingLLMProvider("mock", func(_ context.Context, role string, turn int, req llm.ChatRequest) (*llm.ChatResponse, error) {
		switch role {
		case "correctness":
			if turn == 0 {
				tc := llm.ToolCall{
					ID:        "tc_corr_read",
					Name:      "read_file",
					Arguments: json.RawMessage(`{"path":"internal/auth/service.go","start_line":1,"end_line":30}`),
				}
				return &llm.ChatResponse{
					Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls: []llm.ToolCall{tc},
					Usage:     llm.TokenUsage{InputTokens: 100, OutputTokens: 20},
				}, nil
			}
			tc := llm.ToolCall{
				ID:   "tc_corr_submit",
				Name: "submit_output",
				Arguments: json.RawMessage(`{
					"summary": "Checked logic in service.go; found nil check issue.",
					"findings": [{
						"file_path": "internal/auth/service.go",
						"start_line": 12,
						"end_line": 18,
						"severity": "high",
						"category": "correctness",
						"title": "Unchecked empty token",
						"description": "VerifyToken returns nil even when tok is empty.",
						"confidence": 0.92
					}]
				}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
				Usage:     llm.TokenUsage{InputTokens: 120, OutputTokens: 40},
			}, nil

		case "security":
			if turn == 0 {
				tc := llm.ToolCall{
					ID:        "tc_sec_search",
					Name:      "search_code",
					Arguments: json.RawMessage(`{"pattern":"VerifyToken"}`),
				}
				return &llm.ChatResponse{
					Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls: []llm.ToolCall{tc},
					Usage:     llm.TokenUsage{InputTokens: 90, OutputTokens: 15},
				}, nil
			}
			tc := llm.ToolCall{
				ID:   "tc_sec_submit",
				Name: "submit_output",
				Arguments: json.RawMessage(`{
					"summary": "Audited authentication flow in service.go.",
					"findings": [{
						"file_path": "internal/auth/service.go",
						"start_line": 12,
						"end_line": 18,
						"severity": "critical",
						"category": "security",
						"title": "Authentication bypass via empty token",
						"description": "Empty bearer tokens pass verification without signature check.",
						"confidence": 0.98
					}]
				}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
				Usage:     llm.TokenUsage{InputTokens: 110, OutputTokens: 35},
			}, nil

		case "performance-architecture":
			tc := llm.ToolCall{
				ID:   "tc_perf_submit",
				Name: "submit_output",
				Arguments: json.RawMessage(`{
					"summary": "No performance regressions detected.",
					"findings": []
				}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
				Usage:     llm.TokenUsage{InputTokens: 80, OutputTokens: 20},
			}, nil

		case "test-runner":
			if turn == 0 {
				tc := llm.ToolCall{
					ID:        "tc_test_exec",
					Name:      "exec_command",
					Arguments: json.RawMessage(`{"command":"go test ./..."}`),
				}
				return &llm.ChatResponse{
					Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls: []llm.ToolCall{tc},
					Usage:     llm.TokenUsage{InputTokens: 85, OutputTokens: 15},
				}, nil
			}
			tc := llm.ToolCall{
				ID:   "tc_test_submit",
				Name: "submit_output",
				Arguments: json.RawMessage(`{
					"summary": "All unit tests executed and passed.",
					"findings": [],
					"test_execution": {
						"ran": true,
						"passed": true,
						"commands": ["go test ./..."],
						"summary": "go test ./... passed in sandbox"
					}
				}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
				Usage:     llm.TokenUsage{InputTokens: 105, OutputTokens: 30},
			}, nil

		case "synthesizer":
			if turn == 0 {
				// Return invalid verdict on turn 0 to test Synthesizer schema self-correction loop.
				return &llm.ChatResponse{
					Message: llm.Message{
						Role:    llm.RoleAssistant,
						Content: `{"summary":"Draft","verdict":"INVALID_VERDICT","findings":[]}`,
					},
					Usage: llm.TokenUsage{InputTokens: 200, OutputTokens: 25},
				}, nil
			}
			validReport := `{
				"summary": "Critical authentication bypass in internal/auth/service.go; internal/db/store.go was not inspected.",
				"verdict": "REQUEST_CHANGES",
				"findings": [{
					"file_path": "internal/auth/service.go",
					"start_line": 12,
					"end_line": 18,
					"severity": "critical",
					"category": "security",
					"title": "Authentication bypass via empty token",
					"description": "VerifyToken returns nil without validating token signature or non-empty input.",
					"confidence": 0.98,
					"source_agents": ["correctness", "security"]
				}]
			}`
			return &llm.ChatResponse{
				Message: llm.Message{
					Role:    llm.RoleAssistant,
					Content: "```json\n" + validReport + "\n```",
				},
				Usage: llm.TokenUsage{InputTokens: 240, OutputTokens: 60},
			}, nil
		default:
			return nil, errors.New("unexpected role: " + role)
		}
	})

	sink := &recordingSink{name: "github_pr_review"}
	sinkReg := output.NewRegistry()
	sinkReg.Register(sink)
	sinkReg.Register(&recordingSink{name: "github_check_run"})

	runner := &Runner{
		Store:              st,
		SandboxProvider:    sbProvider,
		DefaultLLMProvider: llmProvider,
		PromptEngine:       promptEngine,
		SinkRegistry:       sinkReg,
	}

	task := &domain.TaskContext{
		RunID:        "run-e2e-1",
		WorkflowName: wf.Name,
		TriggerType:  "github_webhook",
		DedupKey:     "shohei-yamagiwa/mochi:pr:101",
		Workspace: domain.WorkspaceSpec{
			RepoURL: "https://github.com/shohei-yamagiwa/mochi.git",
			Ref:     "abcdef1234567890",
			BaseRef: "1111111122222222",
		},
		Changeset: &domain.ChangesetSpec{
			RepoOwner:  "shohei-yamagiwa",
			RepoName:   "mochi",
			PRNumber:   101,
			Title:      "Add token verification",
			Author:     "octocat",
			BaseBranch: "main",
			HeadBranch: "feat/auth",
			BaseSHA:    "1111111122222222",
			HeadSHA:    "abcdef1234567890",
			ChangedFiles: []domain.ChangedFile{
				{
					Path:      "internal/auth/service.go",
					Status:    "modified",
					Additions: 20,
					Deletions: 2,
					ChangedHunks: []domain.LineRange{
						{StartLine: 10, EndLine: 25},
					},
				},
				{
					Path:      "internal/db/store.go",
					Status:    "modified",
					Additions: 5,
					Deletions: 1,
					ChangedHunks: []domain.LineRange{
						{StartLine: 1, EndLine: 10},
					},
				},
			},
		},
	}

	res, err := runner.Execute(ctx, task, wf)
	if err != nil {
		t.Fatalf("Runner.Execute error = %v", err)
	}

	if res.Run.Status != domain.RunStatusCompleted {
		t.Fatalf("Run.Status = %q, want %q", res.Run.Status, domain.RunStatusCompleted)
	}
	if len(res.Specialists) != 4 {
		t.Fatalf("len(Specialists) = %d, want 4", len(res.Specialists))
	}

	// Verify base sandbox was created once and destroyed cleanly.
	if len(sbProvider.createdBase) != 1 {
		t.Fatalf("len(createdBase) = %d, want 1", len(sbProvider.createdBase))
	}
	if sbProvider.createdBase[0].destroyed != 1 {
		t.Fatalf("baseSandbox.destroyed = %d, want 1", sbProvider.createdBase[0].destroyed)
	}

	// Verify 4 forked sandboxes were created (one per specialist role) and all destroyed.
	if len(sbProvider.forkedByName) != 4 {
		t.Fatalf("len(forkedByName) = %d, want 4", len(sbProvider.forkedByName))
	}
	for name, forked := range sbProvider.forkedByName {
		if forked.destroyed != 1 {
			t.Errorf("forked sandbox %q destroyed = %d, want 1", name, forked.destroyed)
		}
	}

	// Verify rendered system prompts for all 4 specialist roles and synthesizer.
	for _, role := range []string{"correctness", "security", "performance-architecture", "test-runner", "synthesizer"} {
		sys := llmProvider.sysByRole[role]
		if !strings.Contains(sys, "shohei-yamagiwa/mochi") {
			t.Errorf("role %q system prompt did not contain repo name: %q", role, sys)
		}
	}

	// Verify deterministic coverage report (1 of 2 files reviewed -> 0.5).
	if res.Coverage.TotalChangedFiles != 2 || res.Coverage.ReviewedChangedFiles != 1 {
		t.Fatalf("Coverage files = %d/%d, want 1/2", res.Coverage.ReviewedChangedFiles, res.Coverage.TotalChangedFiles)
	}
	if res.Coverage.FileCoverageRatio != 0.5 {
		t.Fatalf("Coverage.FileCoverageRatio = %v, want 0.5", res.Coverage.FileCoverageRatio)
	}
	if len(res.Coverage.UnreviewedFiles) != 1 || res.Coverage.UnreviewedFiles[0] != "internal/db/store.go" {
		t.Fatalf("Coverage.UnreviewedFiles = %v, want [internal/db/store.go]", res.Coverage.UnreviewedFiles)
	}

	// Verify Synthesizer took 2 turns (turn 0 failed validation, turn 1 succeeded) and enriched report.
	if res.SynthesizerRun.TurnsUsed != 2 {
		t.Fatalf("SynthesizerRun.TurnsUsed = %d, want 2", res.SynthesizerRun.TurnsUsed)
	}
	if res.Report.Verdict != output.VerdictRequestChanges {
		t.Fatalf("Report.Verdict = %q, want %q", res.Report.Verdict, output.VerdictRequestChanges)
	}
	if res.Report.TestExecution == nil || !res.Report.TestExecution.Ran || !res.Report.TestExecution.Passed {
		t.Fatalf("Report.TestExecution = %+v, want ran=true passed=true", res.Report.TestExecution)
	}
	if len(res.Report.AgentSummaries) != 4 {
		t.Fatalf("len(Report.AgentSummaries) = %d, want 4", len(res.Report.AgentSummaries))
	}

	// Verify SQLite persistence of Run, CoverageReport, and AgentRuns.
	persistedRun, err := st.GetRun(ctx, task.RunID)
	if err != nil {
		t.Fatalf("st.GetRun: %v", err)
	}
	if persistedRun.Status != domain.RunStatusCompleted {
		t.Fatalf("persistedRun.Status = %q, want %q", persistedRun.Status, domain.RunStatusCompleted)
	}

	persistedCov, err := st.GetCoverageReport(ctx, task.RunID)
	if err != nil {
		t.Fatalf("st.GetCoverageReport: %v", err)
	}
	if persistedCov.FileCoverageRatio != 0.5 {
		t.Fatalf("persistedCov.FileCoverageRatio = %v, want 0.5", persistedCov.FileCoverageRatio)
	}

	agentRuns, err := st.ListAgentRuns(ctx, task.RunID)
	if err != nil {
		t.Fatalf("st.ListAgentRuns: %v", err)
	}
	if len(agentRuns) != 5 { // 4 specialists + 1 synthesizer
		t.Fatalf("len(agentRuns) = %d, want 5", len(agentRuns))
	}

	// Verify OutputSink received OnRunStarted and Publish.
	if sink.started != 1 {
		t.Fatalf("sink.started = %d, want 1", sink.started)
	}
	if len(sink.published) != 1 {
		t.Fatalf("len(sink.published) = %d, want 1", len(sink.published))
	}
}

func TestRunner_SpecialistFailureCleansUpSandboxesAndNotifiesSink(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	repoRoot := filepath.Join("..", "..")
	wf, err := config.LoadWorkflowFile(filepath.Join(repoRoot, "configs", "workflows", "pr-review.yaml"), repoRoot)
	if err != nil {
		t.Fatalf("LoadWorkflowFile: %v", err)
	}

	sbProvider := newTrackingSandboxProvider(nil)
	llmProvider := newRoutingLLMProvider("mock", func(_ context.Context, _ string, _ int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
		return nil, errors.New("provider rate limit exceeded")
	})

	sink := &recordingSink{name: "github_pr_review"}
	sinkReg := output.NewRegistry()
	sinkReg.Register(sink)

	runner := &Runner{
		Store:              st,
		SandboxProvider:    sbProvider,
		DefaultLLMProvider: llmProvider,
		PromptEngine:       config.NewPromptEngine(repoRoot),
		SinkRegistry:       sinkReg,
	}

	task := &domain.TaskContext{
		RunID:        "run-fail-cleanup",
		WorkflowName: wf.Name,
		TriggerType:  "cli",
		DedupKey:     "repo:pr:2",
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			PRNumber:  2,
			BaseSHA:   "1111111",
			HeadSHA:   "2222222",
		},
	}

	_, err = runner.Execute(ctx, task, wf)
	if err == nil || !strings.Contains(err.Error(), "provider rate limit exceeded") {
		t.Fatalf("Runner.Execute error = %v, want provider rate limit exceeded", err)
	}

	// Base sandbox and all forked sandboxes must be destroyed with a non-cancelled context.
	if len(sbProvider.createdBase) != 1 || sbProvider.createdBase[0].destroyed != 1 {
		t.Fatalf("base sandbox was not destroyed on failure")
	}
	if sbProvider.createdBase[0].lastDestroyEr != nil {
		t.Fatalf("base sandbox Destroy received cancelled ctx: %v", sbProvider.createdBase[0].lastDestroyEr)
	}
	for name, forked := range sbProvider.forkedByName {
		if forked.destroyed != 1 {
			t.Errorf("forked sandbox %q destroyed = %d, want 1", name, forked.destroyed)
		}
		if forked.lastDestroyEr != nil {
			t.Errorf("forked sandbox %q Destroy received cancelled ctx: %v", name, forked.lastDestroyEr)
		}
	}

	// RunRecord in SQLite must be marked failed and OutputSink.OnRunFailed must be called.
	persistedRun, err := st.GetRun(ctx, task.RunID)
	if err != nil {
		t.Fatalf("st.GetRun: %v", err)
	}
	if persistedRun.Status != domain.RunStatusFailed {
		t.Fatalf("persistedRun.Status = %q, want %q", persistedRun.Status, domain.RunStatusFailed)
	}
	if len(sink.failedErrs) != 1 {
		t.Fatalf("len(sink.failedErrs) = %d, want 1", len(sink.failedErrs))
	}
}

func TestRunner_PartialSpecialistFailureContinuesWithSurvivingSpecialists(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	repoRoot := filepath.Join("..", "..")
	wf, err := config.LoadWorkflowFile(filepath.Join(repoRoot, "configs", "workflows", "pr-review.yaml"), repoRoot)
	if err != nil {
		t.Fatalf("LoadWorkflowFile: %v", err)
	}

	sbProvider := newTrackingSandboxProvider(nil)
	llmProvider := newRoutingLLMProvider("mock", func(_ context.Context, role string, _ int, req llm.ChatRequest) (*llm.ChatResponse, error) {
		if role == "security" {
			return nil, errors.New("gemini rate limit exceeded")
		}
		if role == "synthesizer" {
			// Verify Synthesizer ResponseSchema omits "coverage".
			if req.ResponseSchema != nil {
				if _, hasCov := req.ResponseSchema.Properties["coverage"]; hasCov {
					t.Errorf("expected synthesizer ResponseSchema to omit coverage property")
				}
			}
			return &llm.ChatResponse{
				Message: llm.Message{
					Role:    llm.RoleAssistant,
					Content: `{"summary":"Completed with 3 of 4 specialists","verdict":"COMMENT","findings":[]}`,
				},
			}, nil
		}
		tc := llm.ToolCall{
			ID:        "tc_ok_" + role,
			Name:      "submit_output",
			Arguments: json.RawMessage(`{"summary":"ok","findings":[]}`),
		}
		return &llm.ChatResponse{
			Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
			ToolCalls: []llm.ToolCall{tc},
		}, nil
	})

	runner := &Runner{
		Store:              st,
		SandboxProvider:    sbProvider,
		DefaultLLMProvider: llmProvider,
		PromptEngine:       config.NewPromptEngine(repoRoot),
	}

	task := &domain.TaskContext{
		RunID:        "run-partial-specialist",
		WorkflowName: wf.Name,
		TriggerType:  "cli",
		DedupKey:     "repo:pr:20",
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			PRNumber:  20,
			BaseSHA:   "1111111",
			HeadSHA:   "2222222",
		},
	}

	res, err := runner.Execute(ctx, task, wf)
	if err != nil {
		t.Fatalf("Runner.Execute error = %v, want nil when 3 of 4 specialists succeed", err)
	}
	if res.Run.Status != domain.RunStatusCompleted {
		t.Fatalf("res.Run.Status = %q, want %q", res.Run.Status, domain.RunStatusCompleted)
	}
	if !strings.Contains(res.Report.AgentSummaries["security"], "failed") {
		t.Fatalf("Report.AgentSummaries[security] = %q, want failure note", res.Report.AgentSummaries["security"])
	}
}

func TestRunner_PartialSinkPublishFailureOnlyNotifiesFailedSink(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	repoRoot := filepath.Join("..", "..")
	wf, err := config.LoadWorkflowFile(filepath.Join(repoRoot, "configs", "workflows", "pr-review.yaml"), repoRoot)
	if err != nil {
		t.Fatalf("LoadWorkflowFile: %v", err)
	}

	sbProvider := newTrackingSandboxProvider(nil)
	llmProvider := newRoutingLLMProvider("mock", func(_ context.Context, role string, _ int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
		if role == "synthesizer" {
			return &llm.ChatResponse{
				Message: llm.Message{
					Role:    llm.RoleAssistant,
					Content: `{"summary":"All clear","verdict":"APPROVE","findings":[]}`,
				},
			}, nil
		}
		tc := llm.ToolCall{
			ID:        "tc_ok_" + role,
			Name:      "submit_output",
			Arguments: json.RawMessage(`{"summary":"ok","findings":[]}`),
		}
		return &llm.ChatResponse{
			Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
			ToolCalls: []llm.ToolCall{tc},
		}, nil
	})

	goodSink := &recordingSink{name: "github_pr_review"}
	badSink := &recordingSink{name: "github_check_run", publishErr: errors.New("webhook 502 bad gateway")}

	runner := &Runner{
		Store:              st,
		SandboxProvider:    sbProvider,
		DefaultLLMProvider: llmProvider,
		PromptEngine:       config.NewPromptEngine(repoRoot),
		Sinks:              []output.OutputSink{goodSink, badSink},
	}

	task := &domain.TaskContext{
		RunID:        "run-partial-sink",
		WorkflowName: wf.Name,
		TriggerType:  "cli",
		DedupKey:     "repo:pr:21",
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			PRNumber:  21,
			BaseSHA:   "1111111",
			HeadSHA:   "2222222",
		},
	}

	res, err := runner.Execute(ctx, task, wf)
	if err == nil || !strings.Contains(err.Error(), "webhook 502 bad gateway") {
		t.Fatalf("Runner.Execute err = %v, want webhook 502 bad gateway", err)
	}
	if res == nil {
		t.Fatalf("expected non-nil RunResult on partial sink publish failure")
	}

	// goodSink published once and must NOT receive OnRunFailed.
	if len(goodSink.published) != 1 {
		t.Fatalf("len(goodSink.published) = %d, want 1", len(goodSink.published))
	}
	if len(goodSink.failedErrs) != 0 {
		t.Fatalf("len(goodSink.failedErrs) = %d, want 0", len(goodSink.failedErrs))
	}

	// badSink failed Publish and received OnRunFailed once.
	if len(badSink.published) != 0 {
		t.Fatalf("len(badSink.published) = %d, want 0", len(badSink.published))
	}
	if len(badSink.failedErrs) != 1 {
		t.Fatalf("len(badSink.failedErrs) = %d, want 1", len(badSink.failedErrs))
	}

	// Stored run remains completed because the artifact was produced and delivered to goodSink.
	persistedRun, err := st.GetRun(ctx, task.RunID)
	if err != nil {
		t.Fatalf("st.GetRun: %v", err)
	}
	if persistedRun.Status != domain.RunStatusCompleted {
		t.Fatalf("persistedRun.Status = %q, want %q", persistedRun.Status, domain.RunStatusCompleted)
	}
}

func TestRunner_RepoOverridesMerge(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	repoRoot := filepath.Join("..", "..")
	wf, err := config.LoadWorkflowFile(filepath.Join(repoRoot, "configs", "workflows", "pr-review.yaml"), repoRoot)
	if err != nil {
		t.Fatalf("LoadWorkflowFile: %v", err)
	}

	sbProvider := newTrackingSandboxProvider(nil)
	llmProvider := newRoutingLLMProvider("mock", func(_ context.Context, role string, _ int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
		if role == "synthesizer" {
			return &llm.ChatResponse{
				Message: llm.Message{
					Role:    llm.RoleAssistant,
					Content: `{"summary":"All clear","verdict":"APPROVE","findings":[]}`,
				},
			}, nil
		}
		tc := llm.ToolCall{
			ID:        "tc_submit_" + role,
			Name:      "submit_output",
			Arguments: json.RawMessage(`{"summary":"ok","findings":[]}`),
		}
		return &llm.ChatResponse{
			Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
			ToolCalls: []llm.ToolCall{tc},
		}, nil
	})

	overrideCalled := false
	runner := &Runner{
		Store:              st,
		SandboxProvider:    sbProvider,
		DefaultLLMProvider: llmProvider,
		PromptEngine:       config.NewPromptEngine(repoRoot),
		RepoOverrideLoader: func(_ context.Context, _ *domain.TaskContext, baseSB sandbox.Sandbox, overridePath string) (*RepoOverrideBundle, error) {
			overrideCalled = true
			if baseSB == nil {
				t.Errorf("expected non-nil baseSandbox in RepoOverrideLoader")
			}
			if overridePath != ".mochi/config.yaml" {
				t.Errorf("overridePath = %q, want .mochi/config.yaml", overridePath)
			}
			// Disable performance-architecture and test-runner via .mochi/config.yaml
			return &RepoOverrideBundle{
				ConfigYAML: []byte(`
agents:
  - role: performance-architecture
    enabled: false
  - role: test-runner
    enabled: false
`),
			}, nil
		},
	}

	task := &domain.TaskContext{
		RunID:        "run-override-1",
		WorkflowName: wf.Name,
		TriggerType:  "cli",
		DedupKey:     "repo:pr:3",
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			PRNumber:  3,
			BaseSHA:   "1111111",
			HeadSHA:   "2222222",
		},
	}

	res, err := runner.Execute(ctx, task, wf)
	if err != nil {
		t.Fatalf("Runner.Execute: %v", err)
	}
	if !overrideCalled {
		t.Fatalf("expected RepoOverrideLoader to be called")
	}
	// Only 2 specialist agents should remain active after merging .mochi/config.yaml.
	if len(res.Specialists) != 2 {
		t.Fatalf("len(res.Specialists) = %d, want 2 (correctness and security)", len(res.Specialists))
	}
	if _, ok := res.Specialists["correctness"]; !ok {
		t.Errorf("expected correctness specialist to run")
	}
	if _, ok := res.Specialists["security"]; !ok {
		t.Errorf("expected security specialist to run")
	}

	// Verify the merged config was persisted to workflow_runs.resolved_config_yaml in SQLite.
	persistedRun, err := st.GetRun(ctx, task.RunID)
	if err != nil {
		t.Fatalf("st.GetRun: %v", err)
	}
	if strings.Contains(persistedRun.ResolvedConfigYAML, "performance-architecture") {
		t.Fatalf("expected persisted resolved_config_yaml to reflect merged override (without performance-architecture), got:\n%s", persistedRun.ResolvedConfigYAML)
	}
	_ = tools.AgentFinding{}
}
