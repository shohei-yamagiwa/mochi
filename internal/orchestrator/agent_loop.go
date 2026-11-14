package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
	"github.com/shohei-yamagiwa/mochi/internal/store"
	"github.com/shohei-yamagiwa/mochi/internal/tools"
)

var (
	// ErrMaxTurnsExceeded is returned when an agent exhausts role.Budgets.MaxTurns without finishing.
	ErrMaxTurnsExceeded = errors.New("agent max_turns budget exceeded")
	// ErrMaxTokensExceeded is returned when an agent exceeds role.Budgets.MaxTokens.
	ErrMaxTokensExceeded = errors.New("agent max_tokens budget exceeded")
)

// AgentLoop executes a single specialist or synthesizer agent across multi-turn LLM + Sandbox tool calls.
type AgentLoop struct {
	Provider llm.Provider
	Store    store.Store
	Registry *tools.Registry
}

// AgentLoopRequest defines the inputs and guardrails for a single AgentLoop execution.
type AgentLoopRequest struct {
	RunID          string
	AgentRunID     string
	Role           domain.RoleConfig
	SystemPrompt   string
	UserPrompt     string
	BaseRef        string
	HeadRef        string
	Sandbox        sandbox.Sandbox
	ToolSet        *tools.ToolSet
	ResponseSchema *jsonschema.Schema
	// RequireSubmitTool enforces termination via the submit_output tool when it is present in the toolset.
	// When nil, defaults to true whenever submit_output is in the toolset.
	RequireSubmitTool *bool
}

// AgentLoopResult captures the completed execution state and final structured output.
type AgentLoopResult struct {
	AgentRun        *domain.AgentRunRecord   `json:"agent_run"`
	SubmittedOutput json.RawMessage          `json:"submitted_output,omitempty"`
	ToolExecutions  []*domain.ToolExecRecord `json:"tool_executions,omitempty"`
}

// Run executes the multi-turn agent loop, enforcing budgets and persisting turns and tool executions.
func (l *AgentLoop) Run(ctx context.Context, req AgentLoopRequest) (*AgentLoopResult, error) {
	if l == nil || l.Provider == nil {
		return nil, fmt.Errorf("agent loop: provider must not be nil")
	}
	if strings.TrimSpace(req.RunID) == "" {
		return nil, fmt.Errorf("agent loop: run_id is required")
	}
	if strings.TrimSpace(req.Role.Role) == "" {
		return nil, fmt.Errorf("agent loop: role name is required")
	}

	toolSet := req.ToolSet
	if toolSet == nil && l.Registry != nil && len(req.Role.Tools) > 0 {
		var err error
		toolSet, err = l.Registry.ScopeForRole(req.Role.Tools)
		if err != nil {
			return nil, fmt.Errorf("agent loop scope tools for role %q: %w", req.Role.Role, err)
		}
	}

	requireSubmit := false
	if req.RequireSubmitTool != nil {
		requireSubmit = *req.RequireSubmitTool
	} else if toolSet != nil && toolSet.HasTool("submit_output") {
		requireSubmit = true
	}

	agentRunID := strings.TrimSpace(req.AgentRunID)
	if agentRunID == "" {
		agentRunID = newID()
	}
	sandboxID := ""
	if req.Sandbox != nil {
		sandboxID = req.Sandbox.ID()
	}
	providerName := req.Role.LLM.Provider
	if providerName == "" {
		providerName = l.Provider.Name()
	}

	startedAt := time.Now().UTC()
	agentRun := &domain.AgentRunRecord{
		AgentRunID:           agentRunID,
		RunID:                req.RunID,
		Role:                 req.Role.Role,
		Provider:             providerName,
		Model:                req.Role.LLM.Model,
		SandboxID:            sandboxID,
		RenderedSystemPrompt: req.SystemPrompt,
		RenderedUserPrompt:   req.UserPrompt,
		Status:               domain.AgentRunStatusRunning,
		StartedAt:            startedAt,
	}

	if l.Store != nil {
		if err := l.Store.CreateAgentRun(ctx, agentRun); err != nil {
			return nil, fmt.Errorf("agent loop create agent run: %w", err)
		}
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Role.Budgets.RunTimeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Role.Budgets.RunTimeout.Std())
		defer cancel()
	}

	var toolExecs []*domain.ToolExecRecord

	finalize := func(status domain.AgentRunStatus, submitted json.RawMessage, runErr error) (*AgentLoopResult, error) {
		now := time.Now().UTC()
		agentRun.Status = status
		agentRun.CompletedAt = &now
		if len(submitted) > 0 {
			agentRun.SubmittedOutputJSON = submitted
		}
		if runErr != nil {
			agentRun.ErrorMessage = runErr.Error()
		}
		if l.Store != nil {
			persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer persistCancel()
			if err := l.Store.UpdateAgentRun(persistCtx, agentRun); err != nil && runErr == nil {
				return nil, fmt.Errorf("agent loop update agent run: %w", err)
			}
		}
		if runErr != nil {
			return &AgentLoopResult{AgentRun: agentRun, SubmittedOutput: submitted, ToolExecutions: toolExecs}, runErr
		}
		return &AgentLoopResult{AgentRun: agentRun, SubmittedOutput: submitted, ToolExecutions: toolExecs}, nil
	}

	messages := []llm.Message{
		{
			Role:    llm.RoleUser,
			Content: req.UserPrompt,
		},
	}

	var toolDefs []llm.ToolDefinition
	if toolSet != nil {
		toolDefs = toolSet.Definitions()
	}

	execCtx := tools.ExecutionContext{
		AgentRole:      req.Role.Role,
		Sandbox:        req.Sandbox,
		BaseRef:        req.BaseRef,
		HeadRef:        req.HeadRef,
		CommandTimeout: req.Role.Budgets.CommandTimeout.Std(),
	}

	nextToolChoice := ""
	if len(toolDefs) > 0 {
		nextToolChoice = "auto"
	}

	maxTurns := req.Role.Budgets.MaxTurns
	maxTokens := req.Role.Budgets.MaxTokens

	for turnIdx := 0; ; turnIdx++ {
		if err := runCtx.Err(); err != nil {
			return finalize(statusFromContextErr(err), nil, err)
		}
		if maxTurns > 0 && agentRun.TurnsUsed >= maxTurns {
			return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("%w (%d)", ErrMaxTurnsExceeded, maxTurns))
		}
		if maxTokens > 0 && (agentRun.InputTokens+agentRun.OutputTokens) >= maxTokens {
			return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("%w (%d/%d)", ErrMaxTokensExceeded, agentRun.InputTokens+agentRun.OutputTokens, maxTokens))
		}

		toolChoice := nextToolChoice
		if maxTurns > 0 && agentRun.TurnsUsed == maxTurns-1 && requireSubmit && toolSet != nil && toolSet.HasTool("submit_output") {
			toolChoice = "submit_output"
		}

		reqMsgsSnapshot := make([]llm.Message, len(messages))
		copy(reqMsgsSnapshot, messages)

		chatReq := llm.ChatRequest{
			Config:         req.Role.LLM,
			SystemPrompt:   req.SystemPrompt,
			Messages:       messages,
			Tools:          toolDefs,
			ToolChoice:     toolChoice,
			ResponseSchema: req.ResponseSchema,
		}

		turnStart := time.Now()
		resp, err := l.Provider.Generate(runCtx, chatReq)
		latencyMS := time.Since(turnStart).Milliseconds()
		if err != nil {
			if ctxErr := runCtx.Err(); ctxErr != nil {
				return finalize(statusFromContextErr(ctxErr), nil, ctxErr)
			}
			return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("llm generate turn %d: %w", turnIdx, err))
		}
		if resp == nil {
			return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("llm generate turn %d: nil response", turnIdx))
		}

		agentRun.TurnsUsed++
		agentRun.InputTokens += resp.Usage.InputTokens
		agentRun.OutputTokens += resp.Usage.OutputTokens
		agentRun.CachedTokens += resp.Usage.CachedTokens

		turnID := newID()
		if l.Store != nil {
			reqJSON, err := json.Marshal(reqMsgsSnapshot)
			if err != nil {
				return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("marshal request messages: %w", err))
			}
			respJSON, err := json.Marshal(resp.Message)
			if err != nil {
				return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("marshal response message: %w", err))
			}
			var toolCallsJSON json.RawMessage
			if len(resp.ToolCalls) > 0 {
				toolCallsJSON, err = json.Marshal(resp.ToolCalls)
				if err != nil {
					return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("marshal tool calls: %w", err))
				}
			}

			turnRec := &domain.LLMTurnRecord{
				TurnID:              turnID,
				AgentRunID:          agentRunID,
				TurnIndex:           turnIdx,
				RequestMessagesJSON: reqJSON,
				ResponseMessageJSON: respJSON,
				ThinkingTrace:       resp.Thinking,
				ToolCallsJSON:       toolCallsJSON,
				InputTokens:         resp.Usage.InputTokens,
				OutputTokens:        resp.Usage.OutputTokens,
				CachedTokens:        resp.Usage.CachedTokens,
				LatencyMS:           latencyMS,
				CreatedAt:           time.Now().UTC(),
			}
			if err := l.Store.SaveLLMTurn(runCtx, turnRec); err != nil {
				if ctxErr := runCtx.Err(); ctxErr != nil {
					return finalize(statusFromContextErr(ctxErr), nil, ctxErr)
				}
				return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("save llm turn %d: %w", turnIdx, err))
			}
		}

		messages = append(messages, resp.Message)

		// Enforce strict token ceiling immediately if we exceeded max_tokens.
		if maxTokens > 0 && (agentRun.InputTokens+agentRun.OutputTokens) > maxTokens {
			return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("%w (%d > %d)", ErrMaxTokensExceeded, agentRun.InputTokens+agentRun.OutputTokens, maxTokens))
		}

		if len(resp.ToolCalls) == 0 {
			if !requireSubmit {
				submitted := normalizeFinalTextOutput(resp.Message.Content)
				return finalize(domain.AgentRunStatusCompleted, submitted, nil)
			}
			if maxTurns > 0 && agentRun.TurnsUsed >= maxTurns {
				return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("%w (%d) without submit_output", ErrMaxTurnsExceeded, maxTurns))
			}
			messages = append(messages, llm.Message{
				Role:    llm.RoleUser,
				Content: "You have not called submit_output yet. Please call submit_output with your validated JSON output now.",
			})
			nextToolChoice = "submit_output"
			continue
		}

		if toolSet == nil {
			return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("agent emitted tool calls but no ToolSet was provided"))
		}

		var submittedOutput json.RawMessage
		submitted := false

		for _, tc := range resp.ToolCalls {
			if err := runCtx.Err(); err != nil {
				return finalize(statusFromContextErr(err), nil, err)
			}

			toolStart := time.Now()
			execRes, execErr := toolSet.Execute(runCtx, execCtx, tc)
			durationMS := time.Since(toolStart).Milliseconds()

			outputText := ""
			exitCode := 0
			var accessed []domain.FileAccessRecord
			if execErr != nil {
				exitCode = 1
				outputText = execErr.Error()
			} else if execRes != nil {
				outputText = execRes.Output
				exitCode = execRes.ExitCode
				accessed = execRes.AccessedFiles
				if execRes.IsFinalOutput {
					submitted = true
					submittedOutput = execRes.SubmittedOutput
				}
			}

			argsJSON := tc.Arguments
			if len(argsJSON) == 0 {
				argsJSON = json.RawMessage("{}")
			}
			toolRec := &domain.ToolExecRecord{
				ToolExecID:    newID(),
				AgentRunID:    agentRunID,
				TurnID:        turnID,
				ToolCallID:    tc.ID,
				ToolName:      tc.Name,
				ArgumentsJSON: argsJSON,
				OutputText:    outputText,
				ExitCode:      exitCode,
				AccessedFiles: accessed,
				DurationMS:    durationMS,
				CreatedAt:     time.Now().UTC(),
			}
			toolExecs = append(toolExecs, toolRec)

			if l.Store != nil {
				if saveErr := l.Store.SaveToolExecution(runCtx, toolRec); saveErr != nil {
					if ctxErr := runCtx.Err(); ctxErr != nil {
						return finalize(statusFromContextErr(ctxErr), nil, ctxErr)
					}
					return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("save tool execution %q: %w", tc.Name, saveErr))
				}
			}

			if execErr != nil {
				if ctxErr := runCtx.Err(); ctxErr != nil {
					return finalize(statusFromContextErr(ctxErr), nil, ctxErr)
				}
				return finalize(domain.AgentRunStatusFailed, nil, fmt.Errorf("execute tool %q: %w", tc.Name, execErr))
			}

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Content:    outputText,
			})
		}

		if submitted {
			return finalize(domain.AgentRunStatusCompleted, submittedOutput, nil)
		}
		nextToolChoice = "auto"
	}
}

func statusFromContextErr(err error) domain.AgentRunStatus {
	if errors.Is(err, context.DeadlineExceeded) {
		return domain.AgentRunStatusTimedOut
	}
	return domain.AgentRunStatusCancelled
}

func normalizeFinalTextOutput(content string) json.RawMessage {
	trimmed := strings.TrimSpace(content)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	encoded, _ := json.Marshal(content)
	return json.RawMessage(encoded)
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
