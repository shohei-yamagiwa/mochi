package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

// ErrNotFound is returned when a requested record does not exist in the store.
var ErrNotFound = errors.New("record not found")

// RunFilter filters workflow run queries.
type RunFilter struct {
	WorkflowName string
	DedupKey     string
	Status       domain.RunStatus
	Limit        int
}

// AgentRunDetail bundles an AgentRunRecord with its recorded LLM turns and tool executions.
type AgentRunDetail struct {
	AgentRun       *domain.AgentRunRecord  `json:"agent_run"`
	LLMTurns       []*domain.LLMTurnRecord `json:"llm_turns"`
	ToolExecutions []*domain.ToolExecRecord `json:"tool_executions"`
}

// RunDetail bundles the full audit trail and coverage report for a workflow run.
type RunDetail struct {
	Run       *domain.RunRecord      `json:"run"`
	Changeset *domain.ChangesetSpec  `json:"changeset,omitempty"`
	AgentRuns []*AgentRunDetail      `json:"agent_runs"`
	Coverage  *domain.CoverageReport `json:"coverage,omitempty"`
}

// Store persists workflow runs, input changeset snapshots, LLM turns, tool executions, and coverage reports.
type Store interface {
	Close() error

	CreateRun(ctx context.Context, run *domain.RunRecord) error
	UpdateRunStatus(ctx context.Context, runID string, status domain.RunStatus, errMsg string, completedAt *time.Time) error
	CompleteRun(ctx context.Context, runID string, status domain.RunStatus, finalArtifact json.RawMessage, completedAt time.Time) error
	GetRun(ctx context.Context, runID string) (*domain.RunRecord, error)
	ListRuns(ctx context.Context, filter RunFilter) ([]*domain.RunRecord, error)

	SaveChangesetSnapshot(ctx context.Context, runID string, changeset *domain.ChangesetSpec) error
	GetChangesetSnapshot(ctx context.Context, runID string) (*domain.ChangesetSpec, error)

	CreateAgentRun(ctx context.Context, agentRun *domain.AgentRunRecord) error
	UpdateAgentRun(ctx context.Context, agentRun *domain.AgentRunRecord) error
	ListAgentRuns(ctx context.Context, runID string) ([]*domain.AgentRunRecord, error)

	SaveLLMTurn(ctx context.Context, turn *domain.LLMTurnRecord) error
	ListLLMTurns(ctx context.Context, agentRunID string) ([]*domain.LLMTurnRecord, error)

	SaveToolExecution(ctx context.Context, exec *domain.ToolExecRecord) error
	ListToolExecutions(ctx context.Context, agentRunID string) ([]*domain.ToolExecRecord, error)
	ListToolExecutionsByRun(ctx context.Context, runID string) ([]*domain.ToolExecRecord, error)

	SaveCoverageReport(ctx context.Context, report *domain.CoverageReport) error
	GetCoverageReport(ctx context.Context, runID string) (*domain.CoverageReport, error)

	GetRunDetail(ctx context.Context, runID string) (*RunDetail, error)
}
