package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
)

type concurrentFakeProvider struct {
	mu   sync.Mutex
	name string
	fn   func(ctx context.Context, turn int, req llm.ChatRequest) (*llm.ChatResponse, error)
	turn int
}

func (p *concurrentFakeProvider) Name() string { return p.name }

func (p *concurrentFakeProvider) Generate(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.mu.Lock()
	t := p.turn
	p.turn++
	p.mu.Unlock()
	return p.fn(ctx, t, req)
}

func minimalSchedulerWorkflow() *domain.Workflow {
	return &domain.Workflow{
		Version: "1",
		Name:    "pr-review",
		Sandbox: domain.SandboxConfig{
			Provider:     "daytona",
			ForkFromBase: false,
		},
		Agents: []domain.RoleConfig{
			{
				Role: "correctness",
				LLM: domain.ModelConfig{
					Provider: "mock",
					Model:    "mock-model",
				},
				Sandbox: domain.AgentSandboxConfig{Enabled: false},
				Tools:   []string{"submit_output"},
				Budgets: domain.BudgetConfig{
					MaxTurns:   2,
					MaxTokens:  10000,
					RunTimeout: domain.Duration(10 * time.Second),
				},
			},
		},
		Synthesizer: domain.SynthesizerConfig{
			LLM: domain.ModelConfig{
				Provider: "mock",
				Model:    "mock-model",
			},
			Budgets: domain.BudgetConfig{
				MaxTurns:   2,
				MaxTokens:  10000,
				RunTimeout: domain.Duration(10 * time.Second),
			},
		},
	}
}

func TestScheduler_SupersedesInFlightRunForSameDedupKey(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	oldStarted := make(chan struct{})
	var oldStartedOnce sync.Once

	provider := &concurrentFakeProvider{
		name: "mock",
		fn: func(callCtx context.Context, _ int, req llm.ChatRequest) (*llm.ChatResponse, error) {
			if req.ResponseSchema != nil {
				return &llm.ChatResponse{
					Message: llm.Message{
						Role:    llm.RoleAssistant,
						Content: `{"summary":"Synthesized","verdict":"APPROVE","findings":[]}`,
					},
				}, nil
			}
			// Specialist call: check if this is the old run by inspecting short SHA in prompt.
			if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "oldsha1") {
				oldStartedOnce.Do(func() { close(oldStarted) })
				<-callCtx.Done()
				return nil, callCtx.Err()
			}
			tc := llm.ToolCall{
				ID:        "tc_submit",
				Name:      "submit_output",
				Arguments: json.RawMessage(`{"summary":"Done","findings":[]}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
			}, nil
		},
	}

	runner := &Runner{
		Store:              st,
		DefaultLLMProvider: provider,
	}
	sched := NewScheduler(runner, SchedulerConfig{MaxWorkers: 2, QueueSize: 16})
	defer func() {
		_ = sched.Shutdown(ctx)
	}()

	wf := minimalSchedulerWorkflow()
	const dedupKey = "shohei-yamagiwa/mochi:pr:55"

	oldTask := &domain.TaskContext{
		RunID:        "run-pr-55-old",
		WorkflowName: wf.Name,
		TriggerType:  "github_webhook",
		DedupKey:     dedupKey,
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			PRNumber:  55,
			BaseSHA:   "basesha",
			HeadSHA:   "oldsha1",
		},
	}

	if _, err := sched.Enqueue(ctx, oldTask, wf); err != nil {
		t.Fatalf("Enqueue(oldTask): %v", err)
	}

	// Wait until oldTask is actively running in the worker.
	select {
	case <-oldStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for oldTask to start")
	}

	if activeID, ok := sched.ActiveRunID(dedupKey); !ok || activeID != oldTask.RunID {
		t.Fatalf("ActiveRunID(%q) = %q, %v; want %q, true", dedupKey, activeID, ok, oldTask.RunID)
	}

	newTask := &domain.TaskContext{
		RunID:        "run-pr-55-new",
		WorkflowName: wf.Name,
		TriggerType:  "github_webhook",
		DedupKey:     dedupKey,
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			PRNumber:  55,
			BaseSHA:   "basesha",
			HeadSHA:   "newsha2",
		},
	}

	if _, err := sched.Enqueue(ctx, newTask, wf); err != nil {
		t.Fatalf("Enqueue(newTask): %v", err)
	}

	// Wait for oldTask to finish with ErrRunSuperseded.
	_, oldErr := sched.WaitForRun(ctx, oldTask.RunID)
	if !errors.Is(oldErr, ErrRunSuperseded) {
		t.Fatalf("WaitForRun(oldTask) err = %v, want ErrRunSuperseded", oldErr)
	}

	// Wait for newTask to finish and succeed.
	newRes, newErr := sched.WaitForRun(ctx, newTask.RunID)
	if newErr != nil {
		t.Fatalf("WaitForRun(newTask) err = %v", newErr)
	}
	if newRes.Run.Status != domain.RunStatusCompleted {
		t.Fatalf("newRes.Run.Status = %q, want %q", newRes.Run.Status, domain.RunStatusCompleted)
	}

	// Verify SQLite records for both runs.
	oldRec, err := st.GetRun(ctx, oldTask.RunID)
	if err != nil {
		t.Fatalf("st.GetRun(old): %v", err)
	}
	if oldRec.Status != domain.RunStatusCancelled {
		t.Fatalf("oldRec.Status = %q, want %q", oldRec.Status, domain.RunStatusCancelled)
	}
	if !strings.Contains(oldRec.ErrorMessage, "superseded by "+newTask.RunID) {
		t.Fatalf("oldRec.ErrorMessage = %q, want mention of superseded by %s", oldRec.ErrorMessage, newTask.RunID)
	}

	newRec, err := st.GetRun(ctx, newTask.RunID)
	if err != nil {
		t.Fatalf("st.GetRun(new): %v", err)
	}
	if newRec.Status != domain.RunStatusCompleted {
		t.Fatalf("newRec.Status = %q, want %q", newRec.Status, domain.RunStatusCompleted)
	}
}

func TestScheduler_SupersedesQueuedRunBeforeWorkerStarts(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	blockerStarted := make(chan struct{})
	var blockerOnce sync.Once
	releaseBlocker := make(chan struct{})

	provider := &concurrentFakeProvider{
		name: "mock",
		fn: func(callCtx context.Context, _ int, req llm.ChatRequest) (*llm.ChatResponse, error) {
			if req.ResponseSchema != nil {
				return &llm.ChatResponse{
					Message: llm.Message{
						Role:    llm.RoleAssistant,
						Content: `{"summary":"OK","verdict":"APPROVE","findings":[]}`,
					},
				}, nil
			}
			if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, "blksha1") {
				blockerOnce.Do(func() { close(blockerStarted) })
				select {
				case <-releaseBlocker:
				case <-callCtx.Done():
					return nil, callCtx.Err()
				}
			}
			tc := llm.ToolCall{
				ID:        "tc_submit",
				Name:      "submit_output",
				Arguments: json.RawMessage(`{"summary":"OK","findings":[]}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
			}, nil
		},
	}

	runner := &Runner{
		Store:              st,
		DefaultLLMProvider: provider,
	}
	// Single worker so queued runs wait while blocker is running.
	sched := NewScheduler(runner, SchedulerConfig{MaxWorkers: 1, QueueSize: 16})
	defer func() {
		_ = sched.Shutdown(ctx)
	}()

	wf := minimalSchedulerWorkflow()

	blockerTask := &domain.TaskContext{
		RunID:        "run-blocker",
		WorkflowName: wf.Name,
		TriggerType:  "cli",
		DedupKey:     "repo:pr:1",
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "shohei-yamagiwa",
			RepoName:  "mochi",
			PRNumber:  1,
			BaseSHA:   "base",
			HeadSHA:   "blksha1",
		},
	}
	if _, err := sched.Enqueue(ctx, blockerTask, wf); err != nil {
		t.Fatalf("Enqueue(blockerTask): %v", err)
	}
	select {
	case <-blockerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for blockerTask to start")
	}

	// Enqueue two runs for PR #2 while worker is occupied.
	queued1 := &domain.TaskContext{
		RunID:        "run-pr2-queued-1",
		WorkflowName: wf.Name,
		TriggerType:  "github_webhook",
		DedupKey:     "repo:pr:2",
		Changeset:    &domain.ChangesetSpec{RepoOwner: "shohei-yamagiwa", RepoName: "mochi", PRNumber: 2, BaseSHA: "b", HeadSHA: "q1"},
	}
	queued2 := &domain.TaskContext{
		RunID:        "run-pr2-queued-2",
		WorkflowName: wf.Name,
		TriggerType:  "github_webhook",
		DedupKey:     "repo:pr:2",
		Changeset:    &domain.ChangesetSpec{RepoOwner: "shohei-yamagiwa", RepoName: "mochi", PRNumber: 2, BaseSHA: "b", HeadSHA: "q2"},
	}

	if _, err := sched.Enqueue(ctx, queued1, wf); err != nil {
		t.Fatalf("Enqueue(queued1): %v", err)
	}
	if _, err := sched.Enqueue(ctx, queued2, wf); err != nil {
		t.Fatalf("Enqueue(queued2): %v", err)
	}

	close(releaseBlocker)

	if _, err := sched.WaitForRun(ctx, blockerTask.RunID); err != nil {
		t.Fatalf("WaitForRun(blocker): %v", err)
	}
	if _, err := sched.WaitForRun(ctx, queued1.RunID); !errors.Is(err, ErrRunSuperseded) {
		t.Fatalf("WaitForRun(queued1) err = %v, want ErrRunSuperseded", err)
	}
	if _, err := sched.WaitForRun(ctx, queued2.RunID); err != nil {
		t.Fatalf("WaitForRun(queued2): %v", err)
	}

	q1Rec, err := st.GetRun(ctx, queued1.RunID)
	if err != nil {
		t.Fatalf("st.GetRun(queued1): %v", err)
	}
	if q1Rec.Status != domain.RunStatusCancelled {
		t.Fatalf("q1Rec.Status = %q, want %q", q1Rec.Status, domain.RunStatusCancelled)
	}

	q2Rec, err := st.GetRun(ctx, queued2.RunID)
	if err != nil {
		t.Fatalf("st.GetRun(queued2): %v", err)
	}
	if q2Rec.Status != domain.RunStatusCompleted {
		t.Fatalf("q2Rec.Status = %q, want %q", q2Rec.Status, domain.RunStatusCompleted)
	}
}

func TestScheduler_CancelByDedupKeyAndShutdown(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	runStarted := make(chan struct{})
	var startOnce sync.Once
	provider := &concurrentFakeProvider{
		name: "mock",
		fn: func(callCtx context.Context, _ int, _ llm.ChatRequest) (*llm.ChatResponse, error) {
			startOnce.Do(func() { close(runStarted) })
			<-callCtx.Done()
			return nil, callCtx.Err()
		},
	}

	sched := NewScheduler(&Runner{Store: st, DefaultLLMProvider: provider}, SchedulerConfig{MaxWorkers: 1})
	wf := minimalSchedulerWorkflow()

	task := &domain.TaskContext{
		RunID:        "run-cancel-dedup",
		WorkflowName: wf.Name,
		TriggerType:  "github_webhook",
		DedupKey:     "repo:pr:77",
	}
	if _, err := sched.Enqueue(ctx, task, wf); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case <-runStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for run to start")
	}

	cancelledID, ok := sched.CancelByDedupKey("repo:pr:77", "PR closed")
	if !ok || cancelledID != task.RunID {
		t.Fatalf("CancelByDedupKey = (%q, %v), want (%q, true)", cancelledID, ok, task.RunID)
	}

	_, waitErr := sched.WaitForRun(ctx, task.RunID)
	if !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("WaitForRun err = %v, want context.Canceled", waitErr)
	}

	if err := sched.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if _, err := sched.Enqueue(ctx, task, wf); !errors.Is(err, ErrSchedulerClosed) {
		t.Fatalf("Enqueue after Shutdown err = %v, want ErrSchedulerClosed", err)
	}
}

func TestScheduler_EnqueueBlockedOnFullQueueRacesWithShutdownSafely(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	jobAStarted := make(chan struct{})
	releaseJobA := make(chan struct{})
	var startOnce sync.Once

	provider := &concurrentFakeProvider{
		name: "mock",
		fn: func(callCtx context.Context, _ int, req llm.ChatRequest) (*llm.ChatResponse, error) {
			if req.ResponseSchema != nil {
				return &llm.ChatResponse{
					Message: llm.Message{
						Role:    llm.RoleAssistant,
						Content: `{"summary":"OK","verdict":"APPROVE","findings":[]}`,
					},
				}, nil
			}
			startOnce.Do(func() { close(jobAStarted) })
			select {
			case <-releaseJobA:
			case <-callCtx.Done():
				return nil, callCtx.Err()
			}
			tc := llm.ToolCall{
				ID:        "tc_submit",
				Name:      "submit_output",
				Arguments: json.RawMessage(`{"summary":"OK","findings":[]}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
			}, nil
		},
	}

	sched := NewScheduler(&Runner{Store: st, DefaultLLMProvider: provider}, SchedulerConfig{MaxWorkers: 1, QueueSize: 1})
	wf := minimalSchedulerWorkflow()

	// Job A occupies the single worker.
	if _, err := sched.Enqueue(ctx, &domain.TaskContext{RunID: "job-a", DedupKey: "repo:pr:1"}, wf); err != nil {
		t.Fatalf("Enqueue(job-a): %v", err)
	}
	<-jobAStarted

	// Job B fills the buffer of size 1.
	if _, err := sched.Enqueue(ctx, &domain.TaskContext{RunID: "job-b", DedupKey: "repo:pr:2"}, wf); err != nil {
		t.Fatalf("Enqueue(job-b): %v", err)
	}

	// Job C blocks in Enqueue because the queue is full.
	jobCErrCh := make(chan error, 1)
	go func() {
		_, err := sched.Enqueue(ctx, &domain.TaskContext{RunID: "job-c", DedupKey: "repo:pr:3"}, wf)
		jobCErrCh <- err
	}()

	time.Sleep(15 * time.Millisecond)

	// Calling Shutdown while Job C is blocked in Enqueue must wake Job C with ErrSchedulerClosed without panicking.
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- sched.Shutdown(ctx)
	}()

	select {
	case cErr := <-jobCErrCh:
		if !errors.Is(cErr, ErrSchedulerClosed) {
			t.Fatalf("Job C Enqueue err = %v, want ErrSchedulerClosed", cErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for blocked Enqueue(job-c) to return on Shutdown")
	}

	close(releaseJobA)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown err = %v", err)
	}
}

func TestScheduler_FullQueueCancelledEnqueueDoesNotSupersedeActiveRun(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	jobAStarted := make(chan struct{})
	releaseJobA := make(chan struct{})
	var startOnce sync.Once

	provider := &concurrentFakeProvider{
		name: "mock",
		fn: func(callCtx context.Context, _ int, req llm.ChatRequest) (*llm.ChatResponse, error) {
			if req.ResponseSchema != nil {
				return &llm.ChatResponse{
					Message: llm.Message{
						Role:    llm.RoleAssistant,
						Content: `{"summary":"OK","verdict":"APPROVE","findings":[]}`,
					},
				}, nil
			}
			startOnce.Do(func() { close(jobAStarted) })
			select {
			case <-releaseJobA:
			case <-callCtx.Done():
				return nil, callCtx.Err()
			}
			tc := llm.ToolCall{
				ID:        "tc_submit",
				Name:      "submit_output",
				Arguments: json.RawMessage(`{"summary":"OK","findings":[]}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
			}, nil
		},
	}

	sched := NewScheduler(&Runner{Store: st, DefaultLLMProvider: provider}, SchedulerConfig{MaxWorkers: 1, QueueSize: 1})
	defer func() { _ = sched.Shutdown(ctx) }()
	wf := minimalSchedulerWorkflow()

	// Job A runs for repo:pr:10.
	if _, err := sched.Enqueue(ctx, &domain.TaskContext{RunID: "job-pr10-active", DedupKey: "repo:pr:10"}, wf); err != nil {
		t.Fatalf("Enqueue(job-pr10-active): %v", err)
	}
	<-jobAStarted

	// Job B fills the queue for repo:pr:20.
	if _, err := sched.Enqueue(ctx, &domain.TaskContext{RunID: "job-pr20-queued", DedupKey: "repo:pr:20"}, wf); err != nil {
		t.Fatalf("Enqueue(job-pr20-queued): %v", err)
	}

	// Attempt to enqueue a new run for repo:pr:10 with a short timeout that expires because the queue is full.
	shortCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = sched.Enqueue(shortCtx, &domain.TaskContext{RunID: "job-pr10-aborted", DedupKey: "repo:pr:10"}, wf)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Enqueue(job-pr10-aborted) err = %v, want DeadlineExceeded", err)
	}

	// The active run for repo:pr:10 must NOT have been superseded!
	close(releaseJobA)
	resA, err := sched.WaitForRun(ctx, "job-pr10-active")
	if err != nil {
		t.Fatalf("WaitForRun(job-pr10-active) err = %v, want nil (not superseded)", err)
	}
	if resA.Run.Status != domain.RunStatusCompleted {
		t.Fatalf("job-pr10-active status = %q, want completed", resA.Run.Status)
	}

	// And the aborted run's SQLite record must be marked cancelled (not stuck in pending).
	abortedRec, err := st.GetRun(ctx, "job-pr10-aborted")
	if err != nil {
		t.Fatalf("st.GetRun(job-pr10-aborted): %v", err)
	}
	if abortedRec.Status != domain.RunStatusCancelled {
		t.Fatalf("abortedRec.Status = %q, want %q", abortedRec.Status, domain.RunStatusCancelled)
	}
}

func TestScheduler_BoundedCompletedJobsEvictionAndStoreFallback(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	provider := &concurrentFakeProvider{
		name: "mock",
		fn: func(_ context.Context, _ int, req llm.ChatRequest) (*llm.ChatResponse, error) {
			if req.ResponseSchema != nil {
				return &llm.ChatResponse{
					Message: llm.Message{
						Role:    llm.RoleAssistant,
						Content: `{"summary":"OK","verdict":"APPROVE","findings":[]}`,
					},
				}, nil
			}
			tc := llm.ToolCall{
				ID:        "tc_submit",
				Name:      "submit_output",
				Arguments: json.RawMessage(`{"summary":"OK","findings":[]}`),
			}
			return &llm.ChatResponse{
				Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{tc}},
				ToolCalls: []llm.ToolCall{tc},
			}, nil
		},
	}

	sched := NewScheduler(&Runner{Store: st, DefaultLLMProvider: provider}, SchedulerConfig{
		MaxWorkers:       1,
		QueueSize:        8,
		MaxCompletedJobs: 2,
	})
	defer func() { _ = sched.Shutdown(ctx) }()
	wf := minimalSchedulerWorkflow()

	for _, id := range []string{"run-1", "run-2", "run-3"} {
		if _, err := sched.Enqueue(ctx, &domain.TaskContext{RunID: id, DedupKey: "repo:pr:" + id}, wf); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
		if _, err := sched.WaitForRun(ctx, id); err != nil {
			t.Fatalf("WaitForRun(%s): %v", id, err)
		}
	}

	// run-1 should have been evicted from in-memory jobsByRunID, yet WaitForRun("run-1") still succeeds via Store fallback.
	sched.mu.Lock()
	_, inMem := sched.jobsByRunID["run-1"]
	jobsLen := len(sched.jobsByRunID)
	sched.mu.Unlock()
	if inMem {
		t.Fatalf("expected run-1 to be evicted from in-memory jobsByRunID")
	}
	if jobsLen > 2 {
		t.Fatalf("len(jobsByRunID) = %d, want <= 2", jobsLen)
	}

	fallbackRes, err := sched.WaitForRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("WaitForRun(run-1) store fallback err = %v", err)
	}
	if fallbackRes.Run.Status != domain.RunStatusCompleted {
		t.Fatalf("fallbackRes.Run.Status = %q, want completed", fallbackRes.Run.Status)
	}
}
