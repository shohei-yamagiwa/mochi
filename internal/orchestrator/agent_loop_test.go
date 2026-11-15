package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
	"github.com/shohei-yamagiwa/mochi/internal/tools"
)

type fakeProvider struct {
	name     string
	requests []llm.ChatRequest
	fn       func(ctx context.Context, turn int, req llm.ChatRequest) (*llm.ChatResponse, error)
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Generate(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	turn := len(f.requests)
	f.requests = append(f.requests, req)
	return f.fn(ctx, turn, req)
}

type fakeSandbox struct {
	id    string
	files map[string]string
}

func (s *fakeSandbox) ID() string                                                { return s.id }
func (s *fakeSandbox) Fork(_ context.Context, _ string) (sandbox.Sandbox, error) { return s, nil }
func (s *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	return &sandbox.ExecResult{ExitCode: 0, Stdout: "ok: " + req.Command}, nil
}
func (s *fakeSandbox) ReadFile(_ context.Context, path string, startLine, endLine int) (string, error) {
	return s.files[path], nil
}
func (s *fakeSandbox) WriteFile(_ context.Context, path string, content []byte) error {
	s.files[path] = string(content)
	return nil
}
func (s *fakeSandbox) ListDir(_ context.Context, _ string) ([]sandbox.FileEntry, error) {
	return []sandbox.FileEntry{{Name: "auth.go", Path: "auth.go"}}, nil
}
func (s *fakeSandbox) SearchFiles(_ context.Context, _, _ string) ([]sandbox.SearchMatch, error) {
	return nil, nil
}
func (s *fakeSandbox) Destroy(_ context.Context) error { return nil }

func setupTestRun(t *testing.T, ctx context.Context, st *sqlite.SQLiteStore, runID string) {
	t.Helper()
	err := st.CreateRun(ctx, &domain.RunRecord{
		RunID:        runID,
		WorkflowName: "pr-review",
		TriggerType:  "cli",
		DedupKey:     "repo:pr:1",
		Status:       domain.RunStatusRunning,
		StartedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

func TestAgentLoop_MultiTurnToolsSelfCorrectionAndPersistence(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	const runID = "run-multi-1"
	setupTestRun(t, ctx, st, runID)

	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("tools.NewRegistry: %v", err)
	}

	provider := &fakeProvider{
		name: "anthropic",
		fn: func(_ context.Context, turn int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
			switch turn {
			case 0:
				tc := llm.ToolCall{
					ID:        "tc_read",
					Name:      "read_file",
					Arguments: json.RawMessage(`{"path":"auth.go","start_line":1,"end_line":10}`),
				}
				return &llm.ChatResponse{
					Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					Thinking:     "Inspecting auth.go lines 1-10",
					ToolCalls:    []llm.ToolCall{tc},
					Usage:        llm.TokenUsage{InputTokens: 100, OutputTokens: 20, CachedTokens: 50},
					FinishReason: "tool_calls",
				}, nil
			case 1:
				tc := llm.ToolCall{
					ID:        "tc_exec",
					Name:      "exec_command",
					Arguments: json.RawMessage(`{"command":"go test ./..."}`),
				}
				return &llm.ChatResponse{
					Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls:    []llm.ToolCall{tc},
					Usage:        llm.TokenUsage{InputTokens: 150, OutputTokens: 15, CachedTokens: 80},
					FinishReason: "tool_calls",
				}, nil
			case 2:
				// Deliberately invalid severity triggers schema self-correction feedback
				tc := llm.ToolCall{
					ID:   "tc_bad_submit",
					Name: "submit_output",
					Arguments: json.RawMessage(`{
						"summary":"oops",
						"findings":[{"file_path":"auth.go","severity":"bogus","category":"security","title":"x","description":"y","confidence":0.9}]
					}`),
				}
				return &llm.ChatResponse{
					Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls:    []llm.ToolCall{tc},
					Usage:        llm.TokenUsage{InputTokens: 170, OutputTokens: 25},
					FinishReason: "tool_calls",
				}, nil
			default:
				tc := llm.ToolCall{
					ID:   "tc_good_submit",
					Name: "submit_output",
					Arguments: json.RawMessage(`{
						"summary":"Found 1 security issue",
						"findings":[{
							"file_path":"auth.go",
							"start_line":4,
							"end_line":8,
							"severity":"high",
							"category":"security",
							"title":"Missing HMAC verify",
							"description":"Webhook secret comparison is skipped",
							"confidence":0.95
						}]
					}`),
				}
				return &llm.ChatResponse{
					Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls:    []llm.ToolCall{tc},
					Usage:        llm.TokenUsage{InputTokens: 200, OutputTokens: 30},
					FinishReason: "tool_calls",
				}, nil
			}
		},
	}

	loop := &AgentLoop{
		Provider: provider,
		Store:    st,
		Registry: reg,
	}

	sb := &fakeSandbox{
		id:    "sb-fork-security",
		files: map[string]string{"auth.go": "package auth\nfunc Verify() {}\n"},
	}

	res, err := loop.Run(ctx, AgentLoopRequest{
		RunID:        runID,
		SystemPrompt: "You are security.",
		UserPrompt:   "Audit auth.go",
		Role: domain.RoleConfig{
			Role: "security",
			LLM: domain.ModelConfig{
				Provider: "anthropic",
				Model:    "claude-opus-5-5",
			},
			Tools: []string{"read_file", "exec_command", "submit_output"},
			Budgets: domain.BudgetConfig{
				MaxTurns:       6,
				MaxTokens:      5000,
				CommandTimeout: domain.Duration(30 * time.Second),
				RunTimeout:     domain.Duration(1 * time.Minute),
			},
		},
		Sandbox: sb,
	})
	if err != nil {
		t.Fatalf("AgentLoop.Run: %v", err)
	}
	if res.AgentRun.Status != domain.AgentRunStatusCompleted {
		t.Fatalf("expected completed status, got %s", res.AgentRun.Status)
	}
	if res.AgentRun.TurnsUsed != 4 {
		t.Fatalf("expected 4 turns used, got %d", res.AgentRun.TurnsUsed)
	}
	if res.AgentRun.InputTokens != 620 || res.AgentRun.OutputTokens != 90 || res.AgentRun.CachedTokens != 130 {
		t.Fatalf("unexpected token totals: %+v", res.AgentRun)
	}

	var parsed tools.AgentFindings
	if err := json.Unmarshal(res.SubmittedOutput, &parsed); err != nil {
		t.Fatalf("unmarshal submitted output: %v", err)
	}
	if len(parsed.Findings) != 1 || parsed.Findings[0].FilePath != "auth.go" {
		t.Fatalf("unexpected submitted findings: %+v", parsed)
	}

	// Verify full SQLite audit trail
	detail, err := st.GetRunDetail(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if len(detail.AgentRuns) != 1 {
		t.Fatalf("expected 1 agent run, got %d", len(detail.AgentRuns))
	}
	ard := detail.AgentRuns[0]
	if len(ard.LLMTurns) != 4 {
		t.Fatalf("expected 4 persisted LLM turns, got %d", len(ard.LLMTurns))
	}
	if ard.LLMTurns[0].ThinkingTrace != "Inspecting auth.go lines 1-10" {
		t.Fatalf("unexpected thinking trace on turn 0: %q", ard.LLMTurns[0].ThinkingTrace)
	}
	if len(ard.ToolExecutions) != 4 {
		t.Fatalf("expected 4 persisted tool executions, got %d", len(ard.ToolExecutions))
	}
	if ard.ToolExecutions[2].ExitCode == 0 || !strings.Contains(ard.ToolExecutions[2].OutputText, "schema validation failed") {
		t.Fatalf("expected rejected submit_output record on tool exec 2, got %+v", ard.ToolExecutions[2])
	}
	if len(ard.ToolExecutions[3].AccessedFiles) != 1 || ard.ToolExecutions[3].AccessedFiles[0].Kind != domain.FileAccessCite {
		t.Fatalf("expected FileAccessCite on final submit_output, got %+v", ard.ToolExecutions[3].AccessedFiles)
	}
}

func TestAgentLoop_NudgeSubmitOutputWhenModelStopsEarly(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()
	setupTestRun(t, ctx, st, "run-nudge")

	reg, _ := tools.NewRegistry()
	provider := &fakeProvider{
		name: "openai",
		fn: func(_ context.Context, turn int, req llm.ChatRequest) (*llm.ChatResponse, error) {
			if turn == 0 {
				return &llm.ChatResponse{
					Message:      llm.Message{Role: llm.RoleAssistant, Content: "Looks fine to me."},
					Usage:        llm.TokenUsage{InputTokens: 30, OutputTokens: 10},
					FinishReason: "stop",
				}, nil
			}
			if req.ToolChoice != "submit_output" {
				t.Fatalf("expected ToolChoice=submit_output on nudge turn, got %q", req.ToolChoice)
			}
			tc := llm.ToolCall{
				ID:        "tc_sub",
				Name:      "submit_output",
				Arguments: json.RawMessage(`{"summary":"Clean","findings":[]}`),
			}
			return &llm.ChatResponse{
				Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls:    []llm.ToolCall{tc},
				Usage:        llm.TokenUsage{InputTokens: 50, OutputTokens: 15},
				FinishReason: "tool_calls",
			}, nil
		},
	}

	loop := &AgentLoop{Provider: provider, Store: st, Registry: reg}
	res, err := loop.Run(ctx, AgentLoopRequest{
		RunID:        "run-nudge",
		SystemPrompt: "Review",
		UserPrompt:   "Go",
		Role: domain.RoleConfig{
			Role:    "correctness",
			LLM:     domain.ModelConfig{Provider: "openai", Model: "gpt-6.1-sol"},
			Tools:   []string{"list_dir", "submit_output"},
			Budgets: domain.BudgetConfig{MaxTurns: 4},
		},
		Sandbox: &fakeSandbox{id: "sb-1"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.AgentRun.Status != domain.AgentRunStatusCompleted || res.AgentRun.TurnsUsed != 2 {
		t.Fatalf("unexpected result: %+v", res.AgentRun)
	}
}

func TestAgentLoop_MaxTurnsAndMaxTokensGuardrails(t *testing.T) {
	ctx := context.Background()
	st, _ := sqlite.Open(ctx, ":memory:")
	defer st.Close()
	setupTestRun(t, ctx, st, "run-budgets")
	reg, _ := tools.NewRegistry()

	// 1. MaxTurns exceeded
	loopTurns := &AgentLoop{
		Provider: &fakeProvider{
			name: "openai",
			fn: func(_ context.Context, turn int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
				tc := llm.ToolCall{ID: "tc", Name: "list_dir", Arguments: json.RawMessage(`{}`)}
				return &llm.ChatResponse{
					Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls: []llm.ToolCall{tc},
					Usage:     llm.TokenUsage{InputTokens: 10, OutputTokens: 5},
				}, nil
			},
		},
		Store:    st,
		Registry: reg,
	}

	resTurns, err := loopTurns.Run(ctx, AgentLoopRequest{
		RunID:      "run-budgets",
		UserPrompt: "Loop forever",
		Role: domain.RoleConfig{
			Role:    "correctness",
			LLM:     domain.ModelConfig{Provider: "openai", Model: "gpt-6.1-sol"},
			Tools:   []string{"list_dir", "submit_output"},
			Budgets: domain.BudgetConfig{MaxTurns: 2},
		},
		Sandbox: &fakeSandbox{id: "sb-1"},
	})
	if !errors.Is(err, ErrMaxTurnsExceeded) {
		t.Fatalf("expected ErrMaxTurnsExceeded, got %v", err)
	}
	if resTurns.AgentRun.Status != domain.AgentRunStatusFailed || resTurns.AgentRun.TurnsUsed != 2 {
		t.Fatalf("unexpected failed agent run: %+v", resTurns.AgentRun)
	}

	// 2. MaxTokens exceeded immediately after turn 0
	loopTokens := &AgentLoop{
		Provider: &fakeProvider{
			name: "openai",
			fn: func(_ context.Context, _ int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
				tc := llm.ToolCall{ID: "tc", Name: "list_dir", Arguments: json.RawMessage(`{}`)}
				return &llm.ChatResponse{
					Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls: []llm.ToolCall{tc},
					Usage:     llm.TokenUsage{InputTokens: 80, OutputTokens: 40},
				}, nil
			},
		},
		Store:    st,
		Registry: reg,
	}

	resTokens, err := loopTokens.Run(ctx, AgentLoopRequest{
		RunID:      "run-budgets",
		UserPrompt: "Spend tokens",
		Role: domain.RoleConfig{
			Role:    "security",
			LLM:     domain.ModelConfig{Provider: "openai", Model: "gpt-6.1-sol"},
			Tools:   []string{"list_dir", "submit_output"},
			Budgets: domain.BudgetConfig{MaxTurns: 5, MaxTokens: 100},
		},
		Sandbox: &fakeSandbox{id: "sb-1"},
	})
	if !errors.Is(err, ErrMaxTokensExceeded) {
		t.Fatalf("expected ErrMaxTokensExceeded, got %v", err)
	}
	if resTokens.AgentRun.Status != domain.AgentRunStatusFailed || resTokens.AgentRun.TurnsUsed != 1 {
		t.Fatalf("unexpected token-exceeded agent run: %+v", resTokens.AgentRun)
	}
}

func TestAgentLoop_RunTimeoutPersistsTimedOutStatus(t *testing.T) {
	ctx := context.Background()
	st, _ := sqlite.Open(ctx, ":memory:")
	defer st.Close()
	setupTestRun(t, ctx, st, "run-timeout")
	reg, _ := tools.NewRegistry()

	loop := &AgentLoop{
		Provider: &fakeProvider{
			name: "gemini",
			fn: func(ctx context.Context, _ int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
		Store:    st,
		Registry: reg,
	}

	res, err := loop.Run(ctx, AgentLoopRequest{
		RunID:      "run-timeout",
		UserPrompt: "Hang",
		Role: domain.RoleConfig{
			Role:    "performance-architecture",
			LLM:     domain.ModelConfig{Provider: "gemini", Model: "gemini-3.8-flash"},
			Tools:   []string{"list_dir", "submit_output"},
			Budgets: domain.BudgetConfig{MaxTurns: 5, RunTimeout: domain.Duration(40 * time.Millisecond)},
		},
		Sandbox: &fakeSandbox{id: "sb-1"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	if res.AgentRun.Status != domain.AgentRunStatusTimedOut {
		t.Fatalf("expected timed_out status, got %s", res.AgentRun.Status)
	}

	runs, err := st.ListAgentRuns(ctx, "run-timeout")
	if err != nil || len(runs) != 1 || runs[0].Status != domain.AgentRunStatusTimedOut {
		t.Fatalf("expected persisted timed_out status in sqlite, got runs=%+v err=%v", runs, err)
	}
}

func TestAgentLoop_MaxTokensWindDownForcesSubmitOutput(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()
	setupTestRun(t, ctx, st, "run-winddown")

	reg, _ := tools.NewRegistry()
	sb := &fakeSandbox{
		id:    "sb-winddown",
		files: map[string]string{"main.go": "package main\nfunc f() {}\n"},
	}

	var seenChoices []string
	var seenToolCounts []int
	provider := &fakeProvider{
		name: "anthropic",
		fn: func(_ context.Context, turn int, req llm.ChatRequest) (*llm.ChatResponse, error) {
			seenChoices = append(seenChoices, req.ToolChoice)
			seenToolCounts = append(seenToolCounts, len(req.Tools))
			if req.ToolChoice == "submit_output" {
				if len(req.Tools) != 1 || req.Tools[0].Name != "submit_output" {
					t.Fatalf("expected wind-down turn tools scoped strictly to [submit_output], got %+v", req.Tools)
				}
				tc := llm.ToolCall{
					ID:        fmt.Sprintf("tc_sub_%d", turn),
					Name:      "submit_output",
					Arguments: json.RawMessage(`{"summary":"Partial findings preserved before token ceiling","findings":[]}`),
				}
				return &llm.ChatResponse{
					Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
					ToolCalls:    []llm.ToolCall{tc},
					Usage:        llm.TokenUsage{InputTokens: 32000, OutputTokens: 500, CachedTokens: 28000},
					FinishReason: "tool_calls",
				}, nil
			}
			// Simulate growing conversation: 12k initial, +4k per turn, 500 output
			inputTokens := 12000 + turn*4000
			tc := llm.ToolCall{
				ID:        fmt.Sprintf("tc_read_%d", turn),
				Name:      "read_file",
				Arguments: json.RawMessage(`{"path":"main.go"}`),
			}
			return &llm.ChatResponse{
				Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls:    []llm.ToolCall{tc},
				Usage:        llm.TokenUsage{InputTokens: inputTokens, OutputTokens: 500, CachedTokens: max(0, inputTokens-4000)},
				FinishReason: "tool_calls",
			}, nil
		},
	}

	loop := &AgentLoop{Provider: provider, Store: st, Registry: reg}
	res, err := loop.Run(ctx, AgentLoopRequest{
		RunID:        "run-winddown",
		SystemPrompt: "Review PR",
		UserPrompt:   "Inspect main.go",
		Role: domain.RoleConfig{
			Role:    "correctness",
			LLM:     domain.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-5-5"},
			Tools:   []string{"read_file", "submit_output"},
			Budgets: domain.BudgetConfig{MaxTurns: 15, MaxTokens: 120000},
		},
		Sandbox: sb,
	})
	if err != nil {
		t.Fatalf("expected wind-down submit_output to succeed, got error: %v", err)
	}
	if res.AgentRun.Status != domain.AgentRunStatusCompleted {
		t.Fatalf("Status = %s, want completed", res.AgentRun.Status)
	}
	if len(seenChoices) != 6 || seenChoices[5] != "submit_output" {
		t.Fatalf("expected wind-down at turn 5, seenChoices=%v toolCounts=%v", seenChoices, seenToolCounts)
	}
	if seenToolCounts[0] != 2 || seenToolCounts[5] != 1 {
		t.Fatalf("expected normal turns to expose 2 tools and wind-down 1 tool, got %v", seenToolCounts)
	}
	var parsed tools.AgentFindings
	if err := json.Unmarshal(res.SubmittedOutput, &parsed); err != nil {
		t.Fatalf("unmarshal submitted output: %v", err)
	}
	if !strings.Contains(parsed.Summary, "Partial findings preserved") {
		t.Fatalf("unexpected submitted summary: %q", parsed.Summary)
	}
}

func TestAgentLoop_MaxTokensWindDownFailsIfNotSubmitted(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()
	setupTestRun(t, ctx, st, "run-winddown-fail")

	reg, _ := tools.NewRegistry()
	provider := &fakeProvider{
		name: "openai",
		fn: func(_ context.Context, turn int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
			tc := llm.ToolCall{
				ID:        fmt.Sprintf("tc_%d", turn),
				Name:      "list_dir",
				Arguments: json.RawMessage(`{}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
				Usage:     llm.TokenUsage{InputTokens: 60, OutputTokens: 10},
			}, nil
		},
	}

	loop := &AgentLoop{Provider: provider, Store: st, Registry: reg}
	res, err := loop.Run(ctx, AgentLoopRequest{
		RunID:      "run-winddown-fail",
		UserPrompt: "Explore",
		Role: domain.RoleConfig{
			Role:    "correctness",
			LLM:     domain.ModelConfig{Provider: "openai", Model: "gpt-6.1-sol"},
			Tools:   []string{"list_dir", "submit_output"},
			Budgets: domain.BudgetConfig{MaxTurns: 5, MaxTokens: 100},
		},
		Sandbox: &fakeSandbox{id: "sb-1"},
	})
	if !errors.Is(err, ErrMaxTokensExceeded) {
		t.Fatalf("expected ErrMaxTokensExceeded when wind-down ignores submit_output, got %v", err)
	}
	if res.AgentRun.Status != domain.AgentRunStatusFailed || res.AgentRun.TurnsUsed != 2 {
		t.Fatalf("expected failed after turn 1 wind-down attempt, got %+v", res.AgentRun)
	}
}

