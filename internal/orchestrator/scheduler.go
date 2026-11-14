package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/config"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/store"
)

const (
	defaultMaxWorkers = 4
	defaultQueueSize  = 128
)

var (
	// ErrSchedulerClosed is returned when Enqueue is called after Shutdown has begun.
	ErrSchedulerClosed = errors.New("scheduler is closed")
	// ErrRunSuperseded is the cancellation cause recorded when a newer run with the same DedupKey arrives.
	ErrRunSuperseded = errors.New("workflow run superseded by newer run for dedup key")
)

// SchedulerConfig configures the async worker pool and queue capacity.
type SchedulerConfig struct {
	MaxWorkers int
	QueueSize  int
}

type scheduledJob struct {
	runID    string
	dedupKey string
	task     *domain.TaskContext
	wf       *domain.Workflow
	ctx      context.Context
	cancel   context.CancelCauseFunc
	done     chan struct{}
	result   *RunResult
	err      error
}

// Scheduler manages an async worker pool and enforces per-DedupKey run deduplication by cancelling
// superseded in-flight or queued runs for the same repository + PR.
type Scheduler struct {
	runner     *Runner
	cfg        SchedulerConfig
	baseCtx    context.Context
	baseCancel context.CancelCauseFunc
	queue      chan *scheduledJob
	wg         sync.WaitGroup

	mu            sync.Mutex
	closed        bool
	activeByRunID map[string]*scheduledJob
	activeByDedup map[string]*scheduledJob
	jobsByRunID   map[string]*scheduledJob
}

// NewScheduler constructs and starts a Scheduler backed by runner.
func NewScheduler(runner *Runner, cfg SchedulerConfig) *Scheduler {
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = defaultMaxWorkers
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}

	baseCtx, baseCancel := context.WithCancelCause(context.Background())
	s := &Scheduler{
		runner:        runner,
		cfg:           cfg,
		baseCtx:       baseCtx,
		baseCancel:    baseCancel,
		queue:         make(chan *scheduledJob, cfg.QueueSize),
		activeByRunID: make(map[string]*scheduledJob),
		activeByDedup: make(map[string]*scheduledJob),
		jobsByRunID:   make(map[string]*scheduledJob),
	}

	for range cfg.MaxWorkers {
		s.wg.Add(1)
		go s.workerLoop()
	}
	return s
}

// Enqueue persists the initial pending RunRecord and ChangesetSnapshot, cancels any superseded active run
// sharing task.DedupKey, and schedules the workflow run on the async worker pool.
func (s *Scheduler) Enqueue(ctx context.Context, task *domain.TaskContext, wf *domain.Workflow) (string, error) {
	if s == nil || s.runner == nil {
		return "", fmt.Errorf("scheduler: runner must not be nil")
	}
	if task == nil {
		return "", fmt.Errorf("scheduler: task context must not be nil")
	}
	if wf == nil {
		return "", fmt.Errorf("scheduler: workflow must not be nil")
	}

	if strings.TrimSpace(task.RunID) == "" {
		task.RunID = newID()
	}
	if strings.TrimSpace(task.WorkflowName) == "" {
		task.WorkflowName = wf.Name
	}

	resolvedYAML, err := config.MarshalWorkflowYAML(wf)
	if err != nil {
		return "", fmt.Errorf("scheduler marshal workflow yaml: %w", err)
	}

	if s.runner.Store != nil {
		if _, err := s.runner.Store.GetRun(ctx, task.RunID); errors.Is(err, store.ErrNotFound) {
			runRec := &domain.RunRecord{
				RunID:              task.RunID,
				WorkflowName:       task.WorkflowName,
				TriggerType:        task.TriggerType,
				DedupKey:           task.DedupKey,
				Status:             domain.RunStatusPending,
				ResolvedConfigYAML: resolvedYAML,
				StartedAt:          time.Now().UTC(),
			}
			if err := s.runner.Store.CreateRun(ctx, runRec); err != nil {
				return "", fmt.Errorf("scheduler create pending run %q: %w", task.RunID, err)
			}
		} else if err != nil {
			return "", fmt.Errorf("scheduler check existing run %q: %w", task.RunID, err)
		}

		if task.Changeset != nil {
			if err := s.runner.Store.SaveChangesetSnapshot(ctx, task.RunID, task.Changeset); err != nil {
				return "", fmt.Errorf("scheduler save changeset snapshot %q: %w", task.RunID, err)
			}
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", ErrSchedulerClosed
	}

	jobCtx, jobCancel := context.WithCancelCause(s.baseCtx)
	dedupKey := strings.TrimSpace(task.DedupKey)
	job := &scheduledJob{
		runID:    task.RunID,
		dedupKey: dedupKey,
		task:     task,
		wf:       wf,
		ctx:      jobCtx,
		cancel:   jobCancel,
		done:     make(chan struct{}),
	}

	if dedupKey != "" {
		if prev, exists := s.activeByDedup[dedupKey]; exists && prev.runID != job.runID {
			prev.cancel(fmt.Errorf("%w: superseded by %s", ErrRunSuperseded, job.runID))
		}
		s.activeByDedup[dedupKey] = job
	}
	s.activeByRunID[job.runID] = job
	s.jobsByRunID[job.runID] = job
	s.mu.Unlock()

	select {
	case s.queue <- job:
		return job.runID, nil
	case <-ctx.Done():
		jobCancel(ctx.Err())
		s.cleanupActiveJob(job)
		return "", ctx.Err()
	}
}

// WaitForRun blocks until the specified runID finishes (or ctx expires) and returns its RunResult and error.
func (s *Scheduler) WaitForRun(ctx context.Context, runID string) (*RunResult, error) {
	s.mu.Lock()
	job, ok := s.jobsByRunID[runID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("run %q not found in scheduler", runID)
	}

	select {
	case <-job.done:
		return job.result, job.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CancelRun cancels an active or queued run by its RunID.
func (s *Scheduler) CancelRun(runID string, reason string) bool {
	s.mu.Lock()
	job, ok := s.activeByRunID[runID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	cause := context.Canceled
	if strings.TrimSpace(reason) != "" {
		cause = fmt.Errorf("%w: %s", context.Canceled, strings.TrimSpace(reason))
	}
	job.cancel(cause)
	return true
}

// CancelByDedupKey cancels the currently active run for dedupKey, if any, and returns its RunID.
func (s *Scheduler) CancelByDedupKey(dedupKey string, reason string) (string, bool) {
	key := strings.TrimSpace(dedupKey)
	if key == "" {
		return "", false
	}
	s.mu.Lock()
	job, ok := s.activeByDedup[key]
	s.mu.Unlock()
	if !ok {
		return "", false
	}
	cause := context.Canceled
	if strings.TrimSpace(reason) != "" {
		cause = fmt.Errorf("%w: %s", context.Canceled, strings.TrimSpace(reason))
	}
	job.cancel(cause)
	return job.runID, true
}

// ActiveRunID returns the RunID currently active for dedupKey, if any.
func (s *Scheduler) ActiveRunID(dedupKey string) (string, bool) {
	key := strings.TrimSpace(dedupKey)
	if key == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.activeByDedup[key]
	if !ok {
		return "", false
	}
	return job.runID, true
}

// Shutdown stops accepting new runs, drains the queue, and waits for workers to finish.
// If ctx expires before workers complete, all remaining in-flight runs are cancelled.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.baseCancel(nil)
		return nil
	case <-ctx.Done():
		s.baseCancel(fmt.Errorf("%w: scheduler shutdown timeout", context.Canceled))
		<-done
		return ctx.Err()
	}
}

func (s *Scheduler) workerLoop() {
	defer s.wg.Done()
	for job := range s.queue {
		s.executeJob(job)
	}
}

func (s *Scheduler) executeJob(job *scheduledJob) {
	defer func() {
		job.cancel(nil)
		s.cleanupActiveJob(job)
		close(job.done)
	}()

	res, err := s.runner.Execute(job.ctx, job.task, job.wf)
	job.result = res
	job.err = err
}

func (s *Scheduler) cleanupActiveJob(job *scheduledJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.activeByRunID[job.runID]; ok && cur == job {
		delete(s.activeByRunID, job.runID)
	}
	if job.dedupKey != "" {
		if cur, ok := s.activeByDedup[job.dedupKey]; ok && cur == job {
			delete(s.activeByDedup, job.dedupKey)
		}
	}
}
