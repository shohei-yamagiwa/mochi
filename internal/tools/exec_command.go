package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
)

const defaultCommandTimeout = 60 * time.Second

type execCommandArgs struct {
	Command        string `json:"command" jsonschema:"Shell command to execute inside the repository sandbox"`
	WorkDir        string `json:"workdir,omitempty" jsonschema:"Optional repository-relative working directory"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Optional per-command timeout override in seconds (clamped to role command_timeout)"`
}

type execCommandTool struct {
	schema   *jsonschema.Schema
	maxBytes int
}

// NewExecCommandTool creates the exec_command sandbox tool.
func NewExecCommandTool() Tool {
	s, _ := jsonschema.For[execCommandArgs](nil)
	return &execCommandTool{
		schema:   s,
		maxBytes: defaultMaxOutputBytes,
	}
}

func (t *execCommandTool) Name() string {
	return "exec_command"
}

func (t *execCommandTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.Name(),
		Description: "Execute a shell command inside the isolated sandbox repository directory. Output is capped at 32 KB (head+tail truncation).",
		Parameters:  t.schema,
	}
}

func (t *execCommandTool) Execute(ctx context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error) {
	if execCtx.Sandbox == nil {
		return nil, fmt.Errorf("exec_command: sandbox is nil")
	}
	var args execCommandArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("invalid arguments: %v", err),
			ExitCode: 1,
		}, nil
	}
	if strings.TrimSpace(args.Command) == "" {
		return &ExecutionResult{
			Output:   "invalid arguments: command is required",
			ExitCode: 1,
		}, nil
	}

	workDir := ""
	if strings.TrimSpace(args.WorkDir) != "" {
		cleanDir, err := normalizeRepoPath(args.WorkDir, true)
		if err != nil {
			return &ExecutionResult{
				Output:   fmt.Sprintf("invalid workdir: %v", err),
				ExitCode: 1,
			}, nil
		}
		if cleanDir != "." {
			workDir = cleanDir
		}
	}

	timeout := execCtx.CommandTimeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	if args.TimeoutSeconds > 0 {
		reqTimeout := time.Duration(args.TimeoutSeconds) * time.Second
		if reqTimeout < timeout {
			timeout = reqTimeout
		}
	}

	res, err := execCtx.Sandbox.Exec(ctx, sandbox.ExecRequest{
		Command: args.Command,
		WorkDir: workDir,
		Timeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("exec_command: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	if res.TimedOut {
		fmt.Fprintf(&b, "timed_out: true\n")
	}
	if res.Stdout != "" {
		fmt.Fprintf(&b, "stdout:\n%s\n", res.Stdout)
	}
	if res.Stderr != "" {
		fmt.Fprintf(&b, "stderr:\n%s\n", res.Stderr)
	}

	return &ExecutionResult{
		Output:   truncateHeadTail(strings.TrimSpace(b.String()), t.maxBytes),
		ExitCode: res.ExitCode,
	}, nil
}
