package domain

import (
	"encoding/json"
	"time"
)

// RunStatus represents the lifecycle state of a workflow run.
type RunStatus string

const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCancelled RunStatus = "cancelled"
)

// AgentRunStatus represents the lifecycle state of an individual specialist agent run.
type AgentRunStatus string

const (
	AgentRunStatusPending   AgentRunStatus = "pending"
	AgentRunStatusRunning   AgentRunStatus = "running"
	AgentRunStatusCompleted AgentRunStatus = "completed"
	AgentRunStatusFailed    AgentRunStatus = "failed"
	AgentRunStatusTimedOut  AgentRunStatus = "timed_out"
	AgentRunStatusCancelled AgentRunStatus = "cancelled"
)

// RunRecord persists a top-level workflow execution record.
type RunRecord struct {
	RunID              string          `json:"run_id"`
	WorkflowName       string          `json:"workflow_name"`
	TriggerType        string          `json:"trigger_type"`
	DedupKey           string          `json:"dedup_key"`
	Status             RunStatus       `json:"status"`
	ResolvedConfigYAML string          `json:"resolved_config_yaml,omitempty"`
	FinalArtifactJSON  json.RawMessage `json:"final_artifact_json,omitempty"`
	ErrorMessage       string          `json:"error_message,omitempty"`
	StartedAt          time.Time       `json:"started_at"`
	CompletedAt        *time.Time      `json:"completed_at,omitempty"`
}

// AgentRunRecord persists a single specialist or synthesizer agent execution.
type AgentRunRecord struct {
	AgentRunID          string          `json:"agent_run_id"`
	RunID               string          `json:"run_id"`
	Role                string          `json:"role"`
	Provider            string          `json:"provider"`
	Model               string          `json:"model"`
	SandboxID           string          `json:"sandbox_id,omitempty"`
	RenderedSystemPrompt string         `json:"rendered_system_prompt,omitempty"`
	RenderedUserPrompt   string         `json:"rendered_user_prompt,omitempty"`
	Status              AgentRunStatus  `json:"status"`
	TurnsUsed           int             `json:"turns_used"`
	InputTokens         int             `json:"input_tokens"`
	OutputTokens        int             `json:"output_tokens"`
	CachedTokens        int             `json:"cached_tokens"`
	SubmittedOutputJSON json.RawMessage `json:"submitted_output_json,omitempty"`
	ErrorMessage        string          `json:"error_message,omitempty"`
	StartedAt           time.Time       `json:"started_at"`
	CompletedAt         *time.Time      `json:"completed_at,omitempty"`
}

// LLMTurnRecord persists a single model request/response turn for an agent run.
type LLMTurnRecord struct {
	TurnID              string          `json:"turn_id"`
	AgentRunID          string          `json:"agent_run_id"`
	TurnIndex           int             `json:"turn_index"`
	RequestMessagesJSON json.RawMessage `json:"request_messages_json"`
	ResponseMessageJSON json.RawMessage `json:"response_message_json"`
	ThinkingTrace       string          `json:"thinking_trace,omitempty"`
	ToolCallsJSON       json.RawMessage `json:"tool_calls_json,omitempty"`
	InputTokens         int             `json:"input_tokens"`
	OutputTokens        int             `json:"output_tokens"`
	CachedTokens        int             `json:"cached_tokens"`
	LatencyMS           int64           `json:"latency_ms"`
	CreatedAt           time.Time       `json:"created_at"`
}

// ToolExecRecord persists a sandbox tool invocation and its extracted file access records.
type ToolExecRecord struct {
	ToolExecID    string             `json:"tool_exec_id"`
	AgentRunID    string             `json:"agent_run_id"`
	TurnID        string             `json:"turn_id,omitempty"`
	ToolCallID    string             `json:"tool_call_id"`
	ToolName      string             `json:"tool_name"`
	ArgumentsJSON json.RawMessage    `json:"arguments_json"`
	OutputText    string             `json:"output_text,omitempty"`
	ExitCode      int                `json:"exit_code"`
	AccessedFiles []FileAccessRecord `json:"accessed_files,omitempty"`
	DurationMS    int64              `json:"duration_ms"`
	CreatedAt     time.Time          `json:"created_at"`
}
