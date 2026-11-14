package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
)

type gitDiffArgs struct {
	Path    string `json:"path,omitempty" jsonschema:"Optional repository-relative file or directory path to filter the diff"`
	BaseRef string `json:"base_ref,omitempty" jsonschema:"Optional base git ref/SHA (defaults to PR base ref)"`
	HeadRef string `json:"head_ref,omitempty" jsonschema:"Optional head git ref/SHA (defaults to PR head ref)"`
}

type gitDiffTool struct {
	schema *jsonschema.Schema
}

// NewGitDiffTool creates the git_diff sandbox tool.
func NewGitDiffTool() Tool {
	s, _ := jsonschema.For[gitDiffArgs](nil)
	return &gitDiffTool{schema: s}
}

func (t *gitDiffTool) Name() string { return "git_diff" }

func (t *gitDiffTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.Name(),
		Description: "Inspect unified git diff between base and head commits inside the sandbox, optionally filtered by a repository-relative path.",
		Parameters:  t.schema,
	}
}

func (t *gitDiffTool) Execute(ctx context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error) {
	if execCtx.Sandbox == nil {
		return nil, fmt.Errorf("git_diff: sandbox is nil")
	}
	var args gitDiffArgs
	if len(strings.TrimSpace(string(rawArgs))) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return &ExecutionResult{
				Output:   fmt.Sprintf("invalid arguments: %v", err),
				ExitCode: 1,
			}, nil
		}
	}

	var cleanPath string
	if strings.TrimSpace(args.Path) != "" {
		p, err := normalizeRepoPath(args.Path, true)
		if err != nil {
			return &ExecutionResult{
				Output:   fmt.Sprintf("invalid path: %v", err),
				ExitCode: 1,
			}, nil
		}
		if p != "." {
			cleanPath = p
		}
	}

	baseRef := strings.TrimSpace(args.BaseRef)
	if baseRef == "" {
		baseRef = strings.TrimSpace(execCtx.BaseRef)
	}
	headRef := strings.TrimSpace(args.HeadRef)
	if headRef == "" {
		headRef = strings.TrimSpace(execCtx.HeadRef)
	}
	if err := validateGitRef(baseRef); err != nil {
		return &ExecutionResult{Output: fmt.Sprintf("invalid base_ref: %v", err), ExitCode: 1}, nil
	}
	if err := validateGitRef(headRef); err != nil {
		return &ExecutionResult{Output: fmt.Sprintf("invalid head_ref: %v", err), ExitCode: 1}, nil
	}

	cmdParts := []string{"git diff"}
	switch {
	case baseRef != "" && headRef != "":
		cmdParts = append(cmdParts, shellQuote(baseRef+".."+headRef))
	case baseRef != "":
		cmdParts = append(cmdParts, shellQuote(baseRef))
	}
	if cleanPath != "" {
		cmdParts = append(cmdParts, "--", shellQuote(cleanPath))
	}

	timeout := execCtx.CommandTimeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}

	res, err := execCtx.Sandbox.Exec(ctx, sandbox.ExecRequest{
		Command: strings.Join(cmdParts, " "),
		Timeout: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("git_diff: %w", err)
	}
	if res.ExitCode != 0 {
		errOut := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
		return &ExecutionResult{
			Output:   truncateHeadTail(fmt.Sprintf("git_diff failed (exit=%d): %s", res.ExitCode, errOut), defaultMaxOutputBytes),
			ExitCode: res.ExitCode,
		}, nil
	}

	diffText := res.Stdout
	var accessed []domain.FileAccessRecord
	if cleanPath != "" {
		accessed = append(accessed, domain.FileAccessRecord{
			AgentRole: execCtx.AgentRole,
			Path:      cleanPath,
			Kind:      domain.FileAccessDiff,
		})
	} else {
		for _, p := range extractDiffFiles(diffText) {
			accessed = append(accessed, domain.FileAccessRecord{
				AgentRole: execCtx.AgentRole,
				Path:      p,
				Kind:      domain.FileAccessDiff,
			})
		}
	}

	out := strings.TrimSpace(diffText)
	if out == "" {
		out = "(empty diff)"
	}

	return &ExecutionResult{
		Output:        truncateHeadTail(out, defaultMaxOutputBytes),
		ExitCode:      0,
		AccessedFiles: accessed,
	}, nil
}

func validateGitRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("ref %q must not start with '-'", ref)
	}
	if strings.ContainsAny(ref, " \t\n\r;") {
		return fmt.Errorf("ref %q contains invalid characters", ref)
	}
	return nil
}

func extractDiffFiles(diff string) []string {
	var files []string
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(diff, "\n") {
		// Parse "diff --git a/foo/bar.go b/foo/bar.go" or "+++ b/foo/bar.go"
		if strings.HasPrefix(line, "diff --git ") {
			rest := strings.TrimPrefix(line, "diff --git ")
			if idx := strings.Index(rest, " b/"); idx >= 0 {
				p := strings.TrimSpace(rest[idx+3:])
				if p != "" && p != "/dev/null" {
					if _, ok := seen[p]; !ok {
						seen[p] = struct{}{}
						files = append(files, p)
					}
				}
			}
		} else if strings.HasPrefix(line, "+++ b/") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "+++ b/"))
			if p != "" && p != "/dev/null" {
				if _, ok := seen[p]; !ok {
					seen[p] = struct{}{}
					files = append(files, p)
				}
			}
		}
	}
	return files
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
