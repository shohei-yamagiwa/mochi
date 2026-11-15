package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/shohei-yamagiwa/mochi/internal/config"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/output"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
	"github.com/shohei-yamagiwa/mochi/internal/store"
	"github.com/shohei-yamagiwa/mochi/internal/tools"
)

const defaultSynthesizerMaxTurns = 3

var errSpecialistSandbox = errors.New("sandbox setup failed")

// SpecialistResult captures the execution record and structured findings from a single specialist role.
type SpecialistResult struct {
	Role           string                   `json:"role"`
	AgentRun       *domain.AgentRunRecord   `json:"agent_run"`
	Findings       *tools.AgentFindings     `json:"findings,omitempty"`
	RawOutput      json.RawMessage          `json:"raw_output,omitempty"`
	ToolExecutions []*domain.ToolExecRecord `json:"tool_executions,omitempty"`
	Error          error                    `json:"-"`
}

// RunResult bundles all artifacts and audit records produced by a workflow execution.
type RunResult struct {
	Run            *domain.RunRecord            `json:"run"`
	Workflow       *domain.Workflow             `json:"workflow"`
	Specialists    map[string]*SpecialistResult `json:"specialists"`
	Coverage       *domain.CoverageReport       `json:"coverage"`
	SynthesizerRun *domain.AgentRunRecord       `json:"synthesizer_run"`
	Report         *output.ReviewReport         `json:"report"`
	ArtifactJSON   json.RawMessage              `json:"artifact_json"`
}

// RepoOverrideBundle contains a repository's .mochi/config.yaml bytes and optional custom prompt files
// loaded from the repository's base branch.
type RepoOverrideBundle struct {
	ConfigYAML []byte
	PromptDir  string
}

// RepoOverrideLoader fetches optional .mochi/ overrides from the target repository's base branch.
type RepoOverrideLoader func(ctx context.Context, task *domain.TaskContext, baseSandbox sandbox.Sandbox, overridePath string) (*RepoOverrideBundle, error)

// Runner orchestrates base sandbox preparation, parallel specialist agent fan-out (Fork + errgroup),
// deterministic diff coverage verification, Synthesizer execution, and OutputSink delivery.
type Runner struct {
	Store               store.Store
	SandboxProvider     sandbox.Provider
	LLMRegistry         *llm.Registry
	DefaultLLMProvider  llm.Provider
	ToolRegistry        *tools.Registry
	PromptEngine        *config.PromptEngine
	SinkRegistry        *output.Registry
	Sinks               []output.OutputSink
	StrictSinks         bool
	FailFastSpecialists bool
	RepoOverrideLoader  RepoOverrideLoader
	Logger              *slog.Logger
}

// Execute runs the full end-to-end lifecycle for a single workflow TaskContext.
func (r *Runner) Execute(ctx context.Context, task *domain.TaskContext, wf *domain.Workflow) (*RunResult, error) {
	if r == nil {
		return nil, fmt.Errorf("runner must not be nil")
	}
	if task == nil {
		return nil, fmt.Errorf("runner: task context must not be nil")
	}
	if wf == nil {
		return nil, fmt.Errorf("runner: workflow must not be nil")
	}

	if strings.TrimSpace(task.RunID) == "" {
		task.RunID = newID()
	}
	if strings.TrimSpace(task.WorkflowName) == "" {
		task.WorkflowName = wf.Name
	}

	activeWf := wf
	resolvedYAML, err := config.MarshalWorkflowYAML(activeWf)
	if err != nil {
		return nil, fmt.Errorf("runner marshal workflow yaml: %w", err)
	}

	startedAt := time.Now().UTC()
	runRec := &domain.RunRecord{
		RunID:              task.RunID,
		WorkflowName:       task.WorkflowName,
		TriggerType:        task.TriggerType,
		DedupKey:           task.DedupKey,
		Status:             domain.RunStatusRunning,
		ResolvedConfigYAML: resolvedYAML,
		StartedAt:          startedAt,
	}

	if err := ctx.Err(); err != nil {
		runErr := contextCauseOrErr(ctx, err)
		_ = r.ensureRunRecordExists(ctx, runRec, task.Changeset)
		r.markRunFailedOrCancelled(ctx, task, runRec, nil, runErr)
		return nil, runErr
	}

	if err := r.ensureRunStarted(ctx, runRec, task.Changeset); err != nil {
		return nil, err
	}

	sinks, err := r.resolveSinks(activeWf)
	if err != nil {
		r.markRunFailedOrCancelled(ctx, task, runRec, nil, err)
		return nil, err
	}

	startedSinks := make([]output.OutputSink, 0, len(sinks))
	for _, s := range sinks {
		if err := s.OnRunStarted(ctx, task); err != nil {
			startErr := fmt.Errorf("output sink %q on_run_started: %w", s.Name(), err)
			r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, startErr)
			return nil, startErr
		}
		startedSinks = append(startedSinks, s)
	}

	toolReg := r.ToolRegistry
	if toolReg == nil {
		toolReg, err = tools.NewRegistry()
		if err != nil {
			r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, err)
			return nil, fmt.Errorf("runner init tool registry: %w", err)
		}
	}

	var baseSandbox sandbox.Sandbox
	var destroyBaseOnce sync.Once
	destroyBase := func() {
		destroyBaseOnce.Do(func() {
			if baseSandbox != nil {
				_ = baseSandbox.Destroy(context.WithoutCancel(ctx))
			}
		})
	}
	defer destroyBase()

	if r.needsBaseSandbox(activeWf) {
		if r.SandboxProvider == nil {
			err := fmt.Errorf("runner: sandbox provider is required when agent sandboxes are enabled")
			r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, err)
			return nil, err
		}
		sb, err := r.createSandbox(ctx, fmt.Sprintf("mochi-%s-base", task.RunID), task.Workspace, activeWf.Sandbox)
		if err != nil {
			r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, err)
			return nil, fmt.Errorf("prepare base sandbox: %w", err)
		}
		baseSandbox = sb
	}

	promptEngine := r.PromptEngine
	if activeWf.RepoOverrides.Enabled && r.RepoOverrideLoader != nil {
		overridePath := strings.TrimSpace(activeWf.RepoOverrides.Path)
		if overridePath == "" {
			overridePath = ".mochi/config.yaml"
		}
		bundle, err := r.RepoOverrideLoader(ctx, task, baseSandbox, overridePath)
		if err != nil {
			r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, err)
			return nil, fmt.Errorf("load repo overrides: %w", err)
		}
		if bundle != nil && len(bundle.ConfigYAML) > 0 {
			mergedWf, err := config.MergeRepoOverrides(activeWf, bundle.ConfigYAML)
			if err != nil {
				r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, err)
				return nil, fmt.Errorf("merge repo overrides: %w", err)
			}
			activeWf = mergedWf
			updatedYAML, err := config.MarshalWorkflowYAML(activeWf)
			if err != nil {
				r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, err)
				return nil, fmt.Errorf("marshal merged workflow yaml: %w", err)
			}
			runRec.ResolvedConfigYAML = updatedYAML
			if r.Store != nil {
				if err := r.Store.UpdateRunResolvedConfig(ctx, task.RunID, updatedYAML); err != nil {
					r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, err)
					return nil, fmt.Errorf("update resolved config yaml: %w", err)
				}
			}
			if bundle.PromptDir != "" && promptEngine != nil {
				promptEngine = promptEngine.WithRepoDir(bundle.PromptDir)
			} else if bundle.PromptDir != "" {
				promptEngine = config.NewPromptEngineWithRoots(".", bundle.PromptDir)
			}
			for i, s := range startedSinks {
				if cfgSink, ok := s.(output.ConfigurableSink); ok {
					for _, sc := range activeWf.Output.Sinks {
						if strings.TrimSpace(sc.Type) == s.Name() {
							startedSinks[i] = cfgSink.WithConfig(sc)
							break
						}
					}
				}
			}
		}
	}

	specialistSlice := make([]*SpecialistResult, len(activeWf.Agents))
	g, gctx := errgroup.WithContext(ctx)

	for i, roleCfg := range activeWf.Agents {
		idx := i
		role := roleCfg
		g.Go(func() error {
			res, err := r.runSpecialistAgent(gctx, task, activeWf, role, baseSandbox, promptEngine, toolReg)
			if err != nil {
				return err
			}
			specialistSlice[idx] = res
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		destroyBase()
		runErr := contextCauseOrErr(ctx, err)
		r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, runErr)
		return nil, runErr
	}

	// Tear down the base sandbox immediately after all specialist agents complete.
	destroyBase()

	specialistsByRole := make(map[string]*SpecialistResult, len(specialistSlice))
	succeededSpecialists := 0
	var specialistErrs []error
	for _, sr := range specialistSlice {
		if sr == nil {
			continue
		}
		specialistsByRole[sr.Role] = sr
		if sr.Error != nil {
			specialistErrs = append(specialistErrs, sr.Error)
		} else {
			succeededSpecialists++
		}
	}

	if len(activeWf.Agents) > 0 && succeededSpecialists == 0 {
		runErr := contextCauseOrErr(ctx, errors.Join(specialistErrs...))
		r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, runErr)
		return nil, runErr
	}

	toolExecs, err := r.collectToolExecutions(ctx, task.RunID, specialistSlice)
	if err != nil {
		r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, err)
		return nil, err
	}

	coverageReport := ComputeCoverageReport(task.RunID, task.Changeset, toolExecs)
	if r.Store != nil {
		if err := r.Store.SaveCoverageReport(ctx, coverageReport); err != nil {
			runErr := contextCauseOrErr(ctx, fmt.Errorf("save coverage report: %w", err))
			r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, runErr)
			return nil, runErr
		}
	}

	synthRun, report, artifactJSON, err := r.runSynthesizer(ctx, task, activeWf, promptEngine, specialistSlice, coverageReport)
	if err != nil {
		runErr := contextCauseOrErr(ctx, err)
		r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, runErr)
		return nil, runErr
	}

	completedAt := time.Now().UTC()
	runRec.Status = domain.RunStatusCompleted
	runRec.FinalArtifactJSON = artifactJSON
	runRec.CompletedAt = &completedAt

	if r.Store != nil {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := r.Store.CompleteRun(persistCtx, task.RunID, domain.RunStatusCompleted, artifactJSON, completedAt); err != nil {
			storeErr := fmt.Errorf("complete run in store: %w", err)
			r.markRunFailedOrCancelled(ctx, task, runRec, startedSinks, storeErr)
			return nil, storeErr
		}
	}

	runRes := &RunResult{
		Run:            runRec,
		Workflow:       activeWf,
		Specialists:    specialistsByRole,
		Coverage:       coverageReport,
		SynthesizerRun: synthRun,
		Report:         report,
		ArtifactJSON:   artifactJSON,
	}

	var publishErrs []error
	var failedSinks []output.OutputSink
	for _, s := range startedSinks {
		if err := s.Publish(ctx, task, artifactJSON); err != nil {
			publishErrs = append(publishErrs, fmt.Errorf("output sink %q publish: %w", s.Name(), err))
			failedSinks = append(failedSinks, s)
		}
	}
	if len(publishErrs) > 0 {
		joinedPubErr := errors.Join(publishErrs...)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, s := range failedSinks {
			_ = s.OnRunFailed(persistCtx, task, joinedPubErr)
		}
		// Only transition the persisted run status to failed if every started sink failed to publish;
		// if at least one sink published the artifact, preserve the completed status.
		if len(failedSinks) == len(startedSinks) {
			r.markRunFailedOrCancelled(ctx, task, runRec, nil, joinedPubErr)
		}
		return runRes, joinedPubErr
	}

	return runRes, nil
}

func (r *Runner) runSpecialistAgent(
	ctx context.Context,
	task *domain.TaskContext,
	wf *domain.Workflow,
	role domain.RoleConfig,
	baseSandbox sandbox.Sandbox,
	promptEngine *config.PromptEngine,
	toolReg *tools.Registry,
) (*SpecialistResult, error) {
	failOrRecord := func(err error, ar *domain.AgentRunRecord, raw json.RawMessage, execs []*domain.ToolExecRecord) (*SpecialistResult, error) {
		roleErr := fmt.Errorf("specialist agent %q: %w", role.Role, err)
		if r.FailFastSpecialists || ctx.Err() != nil {
			return nil, roleErr
		}
		return &SpecialistResult{
			Role:           role.Role,
			AgentRun:       ar,
			RawOutput:      raw,
			ToolExecutions: execs,
			Error:          roleErr,
		}, nil
	}

	var agentSB sandbox.Sandbox
	if role.Sandbox.Enabled {
		if wf.Sandbox.ForkFromBase && baseSandbox != nil {
			forkName := fmt.Sprintf("mochi-%s-%s", task.RunID, role.Role)
			forked, err := baseSandbox.Fork(ctx, forkName)
			if err != nil {
				return failOrRecord(fmt.Errorf("%w: fork sandbox: %w", errSpecialistSandbox, err), nil, nil, nil)
			}
			agentSB = forked
		} else {
			if r.SandboxProvider == nil {
				return failOrRecord(fmt.Errorf("%w: sandbox provider is required", errSpecialistSandbox), nil, nil, nil)
			}
			created, err := r.createSandbox(ctx, fmt.Sprintf("mochi-%s-%s", task.RunID, role.Role), task.Workspace, wf.Sandbox)
			if err != nil {
				return failOrRecord(fmt.Errorf("%w: create sandbox: %w", errSpecialistSandbox, err), nil, nil, nil)
			}
			agentSB = created
		}
		defer func() {
			_ = agentSB.Destroy(context.WithoutCancel(ctx))
		}()
	}

	sysPrompt := ""
	if promptEngine != nil && strings.TrimSpace(role.PromptFile) != "" {
		rendered, err := promptEngine.RenderRole(role, config.NewPromptData(task, wf, &role))
		if err != nil {
			return failOrRecord(fmt.Errorf("render prompt: %w", err), nil, nil, nil)
		}
		sysPrompt = rendered
	}
	userPrompt := BuildSpecialistUserPrompt(task, role)

	provider, err := r.resolveLLMProvider(role.LLM.Provider)
	if err != nil {
		return failOrRecord(fmt.Errorf("resolve llm provider: %w", err), nil, nil, nil)
	}

	toolSet, err := toolReg.ScopeForRole(role.Tools)
	if err != nil {
		return failOrRecord(fmt.Errorf("scope tools: %w", err), nil, nil, nil)
	}

	loop := &AgentLoop{
		Provider: provider,
		Store:    r.Store,
		Registry: toolReg,
	}

	loopRes, err := loop.Run(ctx, AgentLoopRequest{
		RunID:        task.RunID,
		Role:         role,
		SystemPrompt: sysPrompt,
		UserPrompt:   userPrompt,
		BaseRef:      resolveBaseRef(task),
		HeadRef:      resolveHeadRef(task),
		Sandbox:      agentSB,
		ToolSet:      toolSet,
	})
	if err != nil {
		var ar *domain.AgentRunRecord
		var execs []*domain.ToolExecRecord
		if loopRes != nil {
			ar = loopRes.AgentRun
			execs = loopRes.ToolExecutions
		}
		return failOrRecord(err, ar, nil, execs)
	}

	var findings tools.AgentFindings
	if len(loopRes.SubmittedOutput) > 0 {
		if err := json.Unmarshal(loopRes.SubmittedOutput, &findings); err != nil {
			return failOrRecord(fmt.Errorf("decode findings: %w", err), loopRes.AgentRun, loopRes.SubmittedOutput, loopRes.ToolExecutions)
		}
	}
	if findings.Findings == nil {
		findings.Findings = []tools.AgentFinding{}
	}

	return &SpecialistResult{
		Role:           role.Role,
		AgentRun:       loopRes.AgentRun,
		Findings:       &findings,
		RawOutput:      loopRes.SubmittedOutput,
		ToolExecutions: loopRes.ToolExecutions,
	}, nil
}

func (r *Runner) runSynthesizer(
	ctx context.Context,
	task *domain.TaskContext,
	wf *domain.Workflow,
	promptEngine *config.PromptEngine,
	specialists []*SpecialistResult,
	coverage *domain.CoverageReport,
) (*domain.AgentRunRecord, *output.ReviewReport, json.RawMessage, error) {
	validator, err := output.NewReviewReportValidator()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("init review report validator: %w", err)
	}
	synthSchema, err := output.NewSynthesizerResponseSchema()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("init synthesizer response schema: %w", err)
	}

	agentSummaries := make(map[string]string, len(specialists))
	specialistFindings := make(map[string]*tools.AgentFindings, len(specialists))
	var failedAgents []string
	var testExec *output.TestSummary

	for _, sr := range specialists {
		if sr == nil {
			continue
		}
		if sr.Error != nil {
			agentSummaries[sr.Role] = sanitizeSpecialistFailureSummary(sr)
			failedAgents = append(failedAgents, sr.Role)
			continue
		}
		if sr.Findings == nil {
			continue
		}
		agentSummaries[sr.Role] = sr.Findings.Summary
		specialistFindings[sr.Role] = sr.Findings
		if sr.Findings.TestExecution != nil && (testExec == nil || sr.Role == "test-runner") {
			testExec = sr.Findings.TestExecution
		}
	}

	synthData := config.PromptData{
		Task:           config.NewPromptTaskContext(task),
		Workflow:       config.NewPromptWorkflow(wf),
		AgentSummaries: agentSummaries,
		Coverage:       coverage,
		Extra: map[string]any{
			"SpecialistFindings": specialistFindings,
			"TestExecution":      testExec,
		},
	}

	sysPrompt := ""
	if promptEngine != nil && strings.TrimSpace(wf.Synthesizer.PromptFile) != "" {
		rendered, err := promptEngine.RenderSynthesizer(wf.Synthesizer, synthData)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("render synthesizer prompt: %w", err)
		}
		sysPrompt = rendered
	}
	userPrompt := BuildSynthesizerUserPrompt(task, specialists, coverage)

	provider, err := r.resolveLLMProvider(wf.Synthesizer.LLM.Provider)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve synthesizer llm provider: %w", err)
	}

	providerName := wf.Synthesizer.LLM.Provider
	if providerName == "" {
		providerName = provider.Name()
	}

	agentRun := &domain.AgentRunRecord{
		AgentRunID:           newID(),
		RunID:                task.RunID,
		Role:                 "synthesizer",
		Provider:             providerName,
		Model:                wf.Synthesizer.LLM.Model,
		RenderedSystemPrompt: sysPrompt,
		RenderedUserPrompt:   userPrompt,
		Status:               domain.AgentRunStatusRunning,
		StartedAt:            time.Now().UTC(),
	}

	if r.Store != nil {
		if err := r.Store.CreateAgentRun(ctx, agentRun); err != nil {
			return nil, nil, nil, fmt.Errorf("create synthesizer agent run: %w", err)
		}
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if wf.Synthesizer.Budgets.RunTimeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, wf.Synthesizer.Budgets.RunTimeout.Std())
		defer cancel()
	}

	finalizeSynth := func(status domain.AgentRunStatus, submitted json.RawMessage, runErr error) (*domain.AgentRunRecord, error) {
		now := time.Now().UTC()
		agentRun.Status = status
		agentRun.CompletedAt = &now
		if len(submitted) > 0 {
			agentRun.SubmittedOutputJSON = submitted
		}
		if runErr != nil {
			agentRun.ErrorMessage = runErr.Error()
		}
		if r.Store != nil {
			persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer persistCancel()
			if err := r.Store.UpdateAgentRun(persistCtx, agentRun); err != nil && runErr == nil {
				return agentRun, fmt.Errorf("update synthesizer agent run: %w", err)
			}
		}
		return agentRun, runErr
	}

	maxTurns := wf.Synthesizer.Budgets.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultSynthesizerMaxTurns
	}
	maxTokens := wf.Synthesizer.Budgets.MaxTokens

	messages := []llm.Message{
		{
			Role:    llm.RoleUser,
			Content: userPrompt,
		},
	}

	for turnIdx := 0; ; turnIdx++ {
		if err := runCtx.Err(); err != nil {
			ar, _ := finalizeSynth(statusFromContextErr(err), nil, err)
			return ar, nil, nil, err
		}
		if agentRun.TurnsUsed >= maxTurns {
			err := fmt.Errorf("synthesizer: %w (%d)", ErrMaxTurnsExceeded, maxTurns)
			ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, err)
			return ar, nil, nil, err
		}
		if maxTokens > 0 && (agentRun.InputTokens+agentRun.OutputTokens) >= maxTokens {
			err := fmt.Errorf("synthesizer: %w (%d/%d)", ErrMaxTokensExceeded, agentRun.InputTokens+agentRun.OutputTokens, maxTokens)
			ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, err)
			return ar, nil, nil, err
		}

		reqMsgsSnapshot := make([]llm.Message, len(messages))
		copy(reqMsgsSnapshot, messages)

		chatReq := llm.ChatRequest{
			Config:         wf.Synthesizer.LLM,
			SystemPrompt:   sysPrompt,
			Messages:       messages,
			ResponseSchema: synthSchema,
		}

		turnStart := time.Now()
		resp, err := provider.Generate(runCtx, chatReq)
		latencyMS := time.Since(turnStart).Milliseconds()
		if err != nil {
			if ctxErr := runCtx.Err(); ctxErr != nil {
				ar, _ := finalizeSynth(statusFromContextErr(ctxErr), nil, ctxErr)
				return ar, nil, nil, ctxErr
			}
			runErr := fmt.Errorf("synthesizer llm generate turn %d: %w", turnIdx, err)
			ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, runErr)
			return ar, nil, nil, runErr
		}
		if resp == nil {
			runErr := fmt.Errorf("synthesizer llm generate turn %d: nil response", turnIdx)
			ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, runErr)
			return ar, nil, nil, runErr
		}

		agentRun.TurnsUsed++
		agentRun.InputTokens += resp.Usage.InputTokens
		agentRun.OutputTokens += resp.Usage.OutputTokens
		agentRun.CachedTokens += resp.Usage.CachedTokens

		turnID := newID()
		if r.Store != nil {
			reqJSON, _ := json.Marshal(reqMsgsSnapshot)
			respJSON, _ := json.Marshal(resp.Message)
			var toolCallsJSON json.RawMessage
			if len(resp.ToolCalls) > 0 {
				toolCallsJSON, _ = json.Marshal(resp.ToolCalls)
			}
			turnRec := &domain.LLMTurnRecord{
				TurnID:              turnID,
				AgentRunID:          agentRun.AgentRunID,
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
			if err := r.Store.SaveLLMTurn(runCtx, turnRec); err != nil {
				if ctxErr := runCtx.Err(); ctxErr != nil {
					ar, _ := finalizeSynth(statusFromContextErr(ctxErr), nil, ctxErr)
					return ar, nil, nil, ctxErr
				}
				runErr := fmt.Errorf("save synthesizer llm turn %d: %w", turnIdx, err)
				ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, runErr)
				return ar, nil, nil, runErr
			}
		}

		messages = append(messages, resp.Message)

		if maxTokens > 0 && (agentRun.InputTokens+agentRun.OutputTokens) > maxTokens {
			err := fmt.Errorf("synthesizer: %w (%d > %d)", ErrMaxTokensExceeded, agentRun.InputTokens+agentRun.OutputTokens, maxTokens)
			ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, err)
			return ar, nil, nil, err
		}

		rawCandidate := extractSynthesizerJSON(resp)
		report, valErr := validator.ValidateJSON(rawCandidate)
		if valErr != nil {
			if agentRun.TurnsUsed >= maxTurns {
				runErr := fmt.Errorf("synthesizer output validation failed after %d turns: %w", agentRun.TurnsUsed, valErr)
				ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, runErr)
				return ar, nil, nil, runErr
			}
			if len(resp.ToolCalls) > 0 {
				for _, tc := range resp.ToolCalls {
					messages = append(messages, llm.Message{
						Role:       llm.RoleTool,
						ToolCallID: tc.ID,
						ToolName:   tc.Name,
						Content:    fmt.Sprintf("schema validation failed: %v", valErr),
					})
				}
			} else {
				messages = append(messages, llm.Message{
					Role:    llm.RoleUser,
					Content: fmt.Sprintf("Your ReviewReport JSON failed schema validation: %v. Please return a valid JSON object matching the ReviewReport schema.", valErr),
				})
			}
			continue
		}

		enrichReviewReport(report, coverage, agentSummaries, testExec, failedAgents)
		if err := validator.ValidateValue(report); err != nil {
			runErr := fmt.Errorf("validate enriched review report: %w", err)
			ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, runErr)
			return ar, nil, nil, runErr
		}

		artifactJSON, err := json.Marshal(report)
		if err != nil {
			runErr := fmt.Errorf("marshal final review report: %w", err)
			ar, _ := finalizeSynth(domain.AgentRunStatusFailed, nil, runErr)
			return ar, nil, nil, runErr
		}

		ar, err := finalizeSynth(domain.AgentRunStatusCompleted, artifactJSON, nil)
		if err != nil {
			return ar, nil, nil, err
		}
		return ar, report, artifactJSON, nil
	}
}

func sanitizeSpecialistFailureSummary(sr *SpecialistResult) string {
	if sr == nil || sr.Error == nil {
		return "failed"
	}
	if errors.Is(sr.Error, context.DeadlineExceeded) || (sr.AgentRun != nil && sr.AgentRun.Status == domain.AgentRunStatusTimedOut) {
		return "failed (timed_out)"
	}
	if errors.Is(sr.Error, context.Canceled) || (sr.AgentRun != nil && sr.AgentRun.Status == domain.AgentRunStatusCancelled) {
		return "failed (cancelled)"
	}
	if errors.Is(sr.Error, ErrMaxTurnsExceeded) {
		return "failed (max_turns_exceeded)"
	}
	if errors.Is(sr.Error, ErrMaxTokensExceeded) {
		return "failed (max_tokens_exceeded)"
	}
	if errors.Is(sr.Error, errSpecialistSandbox) {
		return "failed (sandbox error)"
	}
	return "failed (provider error)"
}

func enrichReviewReport(
	report *output.ReviewReport,
	coverage *domain.CoverageReport,
	agentSummaries map[string]string,
	testExec *output.TestSummary,
	failedAgents []string,
) {
	if report == nil {
		return
	}
	report.Coverage = coverage
	if report.Findings == nil {
		report.Findings = []output.Finding{}
	}
	for i := range report.Findings {
		if report.Findings[i].SourceAgents == nil {
			report.Findings[i].SourceAgents = []string{}
		}
		if clean := normalizeCoveragePath(report.Findings[i].FilePath); clean != "" {
			report.Findings[i].FilePath = clean
		}
	}
	if len(agentSummaries) > 0 {
		if report.AgentSummaries == nil {
			report.AgentSummaries = make(map[string]string, len(agentSummaries))
		}
		for role, summary := range agentSummaries {
			if strings.TrimSpace(report.AgentSummaries[role]) == "" && strings.TrimSpace(summary) != "" {
				report.AgentSummaries[role] = summary
			}
		}
	}
	if len(failedAgents) > 0 {
		report.FailedAgents = append([]string(nil), failedAgents...)
		if report.AgentSummaries == nil {
			report.AgentSummaries = make(map[string]string, len(failedAgents))
		}
		for _, role := range failedAgents {
			if summary := strings.TrimSpace(agentSummaries[role]); summary != "" {
				report.AgentSummaries[role] = summary
			}
		}
		if report.Verdict == output.VerdictApprove {
			report.Verdict = output.VerdictComment
		}
	}
	if report.TestExecution == nil && testExec != nil {
		report.TestExecution = testExec
	}
}

func extractSynthesizerJSON(resp *llm.ChatResponse) []byte {
	if resp == nil {
		return nil
	}
	for _, tc := range resp.ToolCalls {
		if tc.Name == "submit_output" && len(tc.Arguments) > 0 {
			return tc.Arguments
		}
	}
	if len(resp.ToolCalls) > 0 && len(resp.ToolCalls[0].Arguments) > 0 {
		return resp.ToolCalls[0].Arguments
	}
	return []byte(stripMarkdownJSONFence(resp.Message.Content))
}

func stripMarkdownJSONFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	firstNewline := strings.IndexByte(trimmed, '\n')
	if firstNewline < 0 {
		return trimmed
	}
	body := strings.TrimSpace(trimmed[firstNewline+1:])
	if strings.HasSuffix(body, "```") {
		body = strings.TrimSpace(strings.TrimSuffix(body, "```"))
	}
	return body
}

// BuildSpecialistUserPrompt constructs the initial user prompt supplying changeset metadata,
// changed file summaries, and diff context to a specialist agent.
func BuildSpecialistUserPrompt(task *domain.TaskContext, role domain.RoleConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Specialist Role: `%s`\n\n", role.Role)
	if task == nil || task.Changeset == nil {
		b.WriteString("Inspect the repository workspace using your available sandbox tools and submit your findings via `submit_output`.\n")
		return b.String()
	}

	cs := task.Changeset
	if cs.RepoOwner != "" || cs.RepoName != "" {
		fmt.Fprintf(&b, "- **Repository**: `%s/%s`\n", cs.RepoOwner, cs.RepoName)
	}
	if cs.PRNumber > 0 {
		fmt.Fprintf(&b, "- **Pull Request**: #%d", cs.PRNumber)
		if cs.Title != "" {
			fmt.Fprintf(&b, " — %s", cs.Title)
		}
		b.WriteByte('\n')
	} else if cs.Title != "" {
		fmt.Fprintf(&b, "- **Title**: %s\n", cs.Title)
	}
	if cs.Author != "" {
		fmt.Fprintf(&b, "- **Author**: `%s`\n", cs.Author)
	}
	if cs.BaseSHA != "" || cs.HeadSHA != "" {
		fmt.Fprintf(&b, "- **Base..Head**: `%s..%s` (branches: `%s` <- `%s`)\n",
			shortRef(cs.BaseSHA), shortRef(cs.HeadSHA), cs.BaseBranch, cs.HeadBranch)
	}
	if strings.TrimSpace(cs.Description) != "" {
		fmt.Fprintf(&b, "\n### Pull Request Description\n%s\n", strings.TrimSpace(cs.Description))
	}

	if len(cs.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "\n### Changed Files (%d)\n", len(cs.ChangedFiles))
		for _, cf := range cs.ChangedFiles {
			fmt.Fprintf(&b, "- `%s` (%s, +%d/-%d)\n", cf.Path, cf.Status, cf.Additions, cf.Deletions)
		}
	}

	if strings.TrimSpace(cs.UnifiedDiff) != "" {
		b.WriteString("\n### Unified Diff\n```diff\n")
		b.WriteString(truncatePromptDiff(cs.UnifiedDiff, 24000))
		b.WriteString("\n```\n")
	}

	b.WriteString("\nUse your sandbox tools to inspect the changed files and surrounding code context, then call `submit_output` with your verified findings.\n")
	return b.String()
}

// BuildSynthesizerUserPrompt constructs the user prompt for the Synthesizer agent, embedding
// the changeset summary, deterministic CoverageReport, and all specialist agent outputs.
func BuildSynthesizerUserPrompt(
	task *domain.TaskContext,
	specialists []*SpecialistResult,
	coverage *domain.CoverageReport,
) string {
	var b strings.Builder
	b.WriteString("## Code Review Synthesis Request\n\n")

	if task != nil && task.Changeset != nil {
		cs := task.Changeset
		fmt.Fprintf(&b, "- **Repository**: `%s/%s`\n", cs.RepoOwner, cs.RepoName)
		if cs.PRNumber > 0 {
			fmt.Fprintf(&b, "- **Pull Request**: #%d — %s\n", cs.PRNumber, cs.Title)
		}
		if cs.BaseSHA != "" || cs.HeadSHA != "" {
			fmt.Fprintf(&b, "- **Commits**: `%s..%s`\n", shortRef(cs.BaseSHA), shortRef(cs.HeadSHA))
		}
		if len(cs.ChangedFiles) > 0 {
			fmt.Fprintf(&b, "- **Changed Files (%d)**:\n", len(cs.ChangedFiles))
			for _, cf := range cs.ChangedFiles {
				fmt.Fprintf(&b, "  - `%s` (%s, +%d/-%d)\n", cf.Path, cf.Status, cf.Additions, cf.Deletions)
			}
		}
		if strings.TrimSpace(cs.UnifiedDiff) != "" {
			b.WriteString("\n### PR Unified Diff\n```diff\n")
			b.WriteString(truncatePromptDiff(cs.UnifiedDiff, 24000))
			b.WriteString("\n```\n")
		}
	}

	if coverage != nil {
		covJSON, _ := json.MarshalIndent(coverage, "", "  ")
		b.WriteString("\n### Deterministic Diff Coverage Report\n```json\n")
		b.Write(covJSON)
		b.WriteString("\n```\n")
	}

	b.WriteString("\n### Specialist Agent Outputs\n")
	for _, sr := range specialists {
		if sr == nil {
			continue
		}
		fmt.Fprintf(&b, "\n#### Role: `%s`\n```json\n", sr.Role)
		if sr.Error != nil {
			errPayload, _ := json.MarshalIndent(map[string]string{
				"status": "failed",
				"reason": sanitizeSpecialistFailureSummary(sr),
			}, "", "  ")
			b.Write(errPayload)
		} else if sr.Findings != nil {
			raw, _ := json.MarshalIndent(sr.Findings, "", "  ")
			b.Write(raw)
		} else if len(sr.RawOutput) > 0 {
			b.Write(sr.RawOutput)
		} else {
			b.WriteString("{}")
		}
		b.WriteString("\n```\n")
	}

	b.WriteString("\nSynthesize the specialist outputs and coverage report into a single validated `ReviewReport` JSON object (omit `coverage`; Mochi injects the deterministic coverage object automatically).\n")
	return b.String()
}

func (r *Runner) ensureRunRecordExists(ctx context.Context, runRec *domain.RunRecord, cs *domain.ChangesetSpec) error {
	if r.Store == nil {
		return nil
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if _, err := r.Store.GetRun(persistCtx, runRec.RunID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := r.Store.CreateRun(persistCtx, runRec); err != nil {
			return err
		}
	}
	if cs != nil {
		_ = r.Store.SaveChangesetSnapshot(persistCtx, runRec.RunID, cs)
	}
	return nil
}

func (r *Runner) ensureRunStarted(ctx context.Context, runRec *domain.RunRecord, cs *domain.ChangesetSpec) error {
	if r.Store == nil {
		return nil
	}
	existing, err := r.Store.GetRun(ctx, runRec.RunID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("runner get run %q: %w", runRec.RunID, err)
		}
		if err := r.Store.CreateRun(ctx, runRec); err != nil {
			return fmt.Errorf("runner create run %q: %w", runRec.RunID, err)
		}
	} else {
		runRec.StartedAt = existing.StartedAt
		if err := r.Store.UpdateRunStatus(ctx, runRec.RunID, domain.RunStatusRunning, "", nil); err != nil {
			return fmt.Errorf("runner update run %q to running: %w", runRec.RunID, err)
		}
	}

	if cs != nil {
		if err := r.Store.SaveChangesetSnapshot(ctx, runRec.RunID, cs); err != nil {
			return fmt.Errorf("runner save changeset snapshot %q: %w", runRec.RunID, err)
		}
	}
	return nil
}

func (r *Runner) markRunFailedOrCancelled(
	ctx context.Context,
	task *domain.TaskContext,
	runRec *domain.RunRecord,
	startedSinks []output.OutputSink,
	runErr error,
) {
	now := time.Now().UTC()
	status := domain.RunStatusFailed
	if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		status = domain.RunStatusCancelled
	}
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	runRec.Status = status
	runRec.ErrorMessage = errMsg
	runRec.CompletedAt = &now

	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if r.Store != nil {
		_ = r.Store.UpdateRunStatus(persistCtx, runRec.RunID, status, errMsg, &now)
	}
	if task == nil {
		task = &domain.TaskContext{
			RunID:        runRec.RunID,
			WorkflowName: runRec.WorkflowName,
			TriggerType:  runRec.TriggerType,
			DedupKey:     runRec.DedupKey,
		}
	}
	for _, s := range startedSinks {
		_ = s.OnRunFailed(persistCtx, task, runErr)
	}
}

func (r *Runner) resolveSinks(wf *domain.Workflow) ([]output.OutputSink, error) {
	var resolved []output.OutputSink
	seen := make(map[string]struct{})

	for _, sc := range wf.Output.Sinks {
		sinkType := strings.TrimSpace(sc.Type)
		if sinkType == "" {
			continue
		}
		if r.SinkRegistry != nil {
			if s, ok := r.SinkRegistry.Get(sinkType); ok {
				if _, dup := seen[s.Name()]; !dup {
					seen[s.Name()] = struct{}{}
					if cfgSink, ok := s.(output.ConfigurableSink); ok {
						s = cfgSink.WithConfig(sc)
					}
					resolved = append(resolved, s)
				}
				continue
			}
		}
		if r.StrictSinks {
			return nil, fmt.Errorf("output sink %q is not registered", sinkType)
		}
	}

	for _, s := range r.Sinks {
		if s == nil {
			continue
		}
		if _, dup := seen[s.Name()]; !dup {
			seen[s.Name()] = struct{}{}
			resolved = append(resolved, s)
		}
	}
	return resolved, nil
}

func (r *Runner) resolveLLMProvider(providerName string) (llm.Provider, error) {
	name := strings.TrimSpace(providerName)
	if r.LLMRegistry != nil && name != "" {
		if p, ok := r.LLMRegistry.Get(name); ok {
			return p, nil
		}
	}
	if r.DefaultLLMProvider != nil {
		if name == "" || r.DefaultLLMProvider.Name() == name || r.LLMRegistry == nil {
			return r.DefaultLLMProvider, nil
		}
	}
	return nil, fmt.Errorf("llm provider %q is not registered", name)
}

func (r *Runner) needsBaseSandbox(wf *domain.Workflow) bool {
	if !wf.Sandbox.ForkFromBase {
		return false
	}
	for _, a := range wf.Agents {
		if a.Sandbox.Enabled {
			return true
		}
	}
	return false
}

func (r *Runner) createSandbox(
	ctx context.Context,
	name string,
	ws domain.WorkspaceSpec,
	sbCfg domain.SandboxConfig,
) (sandbox.Sandbox, error) {
	effectiveWS := ws
	if effectiveWS.Snapshot == "" && effectiveWS.Image == "" {
		if sbCfg.DefaultSnapshot != "" {
			effectiveWS.Snapshot = sbCfg.DefaultSnapshot
		} else if sbCfg.DefaultImage != "" {
			effectiveWS.Image = sbCfg.DefaultImage
		}
	}

	setupCtx := ctx
	var cancel context.CancelFunc
	if sbCfg.SetupTimeout > 0 {
		setupCtx, cancel = context.WithTimeout(ctx, sbCfg.SetupTimeout.Std())
		defer cancel()
	}

	return r.SandboxProvider.CreateAndPrepareBaseSandbox(setupCtx, sandbox.CreateOptions{
		Name:      name,
		Workspace: effectiveWS,
	})
}

func (r *Runner) collectToolExecutions(
	ctx context.Context,
	runID string,
	specialists []*SpecialistResult,
) ([]*domain.ToolExecRecord, error) {
	if r.Store != nil {
		execs, err := r.Store.ListToolExecutionsByRun(ctx, runID)
		if err != nil {
			return nil, fmt.Errorf("list tool executions for coverage: %w", err)
		}
		return execs, nil
	}
	var execs []*domain.ToolExecRecord
	for _, sr := range specialists {
		if sr != nil && len(sr.ToolExecutions) > 0 {
			execs = append(execs, sr.ToolExecutions...)
		}
	}
	return execs, nil
}

func resolveBaseRef(task *domain.TaskContext) string {
	if task == nil {
		return ""
	}
	if task.Changeset != nil {
		if strings.TrimSpace(task.Changeset.BaseSHA) != "" {
			return strings.TrimSpace(task.Changeset.BaseSHA)
		}
		if strings.TrimSpace(task.Changeset.BaseBranch) != "" {
			return strings.TrimSpace(task.Changeset.BaseBranch)
		}
	}
	return strings.TrimSpace(task.Workspace.BaseRef)
}

func resolveHeadRef(task *domain.TaskContext) string {
	if task == nil {
		return ""
	}
	if task.Changeset != nil {
		if strings.TrimSpace(task.Changeset.HeadSHA) != "" {
			return strings.TrimSpace(task.Changeset.HeadSHA)
		}
		if strings.TrimSpace(task.Changeset.HeadBranch) != "" {
			return strings.TrimSpace(task.Changeset.HeadBranch)
		}
	}
	return strings.TrimSpace(task.Workspace.Ref)
}

func shortRef(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func truncatePromptDiff(diff string, maxBytes int) string {
	if maxBytes <= 0 || len(diff) <= maxBytes {
		return diff
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(diff[end]) {
		end--
	}
	return diff[:end] + "\n... [diff truncated; use git_diff or read_file to inspect full contents]"
}

func contextCauseOrErr(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && (errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)) {
		return cause
	}
	return err
}
