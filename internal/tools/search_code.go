package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
)

const maxSearchMatches = 200

type searchCodeArgs struct {
	Path    string `json:"path,omitempty" jsonschema:"Optional repository-relative directory or file path to search (defaults to '.')"`
	Pattern string `json:"pattern" jsonschema:"Text or regex pattern to search for in file contents"`
}

type searchCodeTool struct {
	schema *jsonschema.Schema
}

// NewSearchCodeTool creates the search_code sandbox tool.
func NewSearchCodeTool() Tool {
	s, _ := jsonschema.For[searchCodeArgs](nil)
	return &searchCodeTool{schema: s}
}

func (t *searchCodeTool) Name() string { return "search_code" }

func (t *searchCodeTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.Name(),
		Description: "Search repository source files for text or regex pattern matches, returning file paths and 1-indexed line numbers.",
		Parameters:  t.schema,
	}
}

func (t *searchCodeTool) Execute(ctx context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error) {
	if execCtx.Sandbox == nil {
		return nil, fmt.Errorf("search_code: sandbox is nil")
	}
	var args searchCodeArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("invalid arguments: %v", err),
			ExitCode: 1,
		}, nil
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return &ExecutionResult{
			Output:   "invalid arguments: pattern is required",
			ExitCode: 1,
		}, nil
	}
	cleanPath, err := normalizeRepoPath(args.Path, true)
	if err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("invalid path: %v", err),
			ExitCode: 1,
		}, nil
	}

	matches, err := execCtx.Sandbox.SearchFiles(ctx, cleanPath, args.Pattern)
	if err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("search_code error: %v", err),
			ExitCode: 1,
		}, nil
	}

	truncated := false
	if len(matches) > maxSearchMatches {
		matches = matches[:maxSearchMatches]
		truncated = true
	}

	var b strings.Builder
	accessed := make([]domain.FileAccessRecord, 0, len(matches))
	for _, m := range matches {
		matchPath := strings.TrimPrefix(strings.ReplaceAll(m.File, `\`, "/"), "./")
		fmt.Fprintf(&b, "%s:%d: %s\n", matchPath, m.LineNumber, m.LineText)
		if matchPath != "" {
			var lr *domain.LineRange
			if m.LineNumber > 0 {
				lr = &domain.LineRange{StartLine: m.LineNumber, EndLine: m.LineNumber}
			}
			accessed = append(accessed, domain.FileAccessRecord{
				AgentRole: execCtx.AgentRole,
				Path:      matchPath,
				Kind:      domain.FileAccessSearch,
				LineRange: lr,
			})
		}
	}
	if len(matches) == 0 {
		b.WriteString("no matches found")
	} else if truncated {
		fmt.Fprintf(&b, "... [showing first %d matches]\n", maxSearchMatches)
	}

	return &ExecutionResult{
		Output:        truncateHeadTail(strings.TrimSpace(b.String()), defaultMaxOutputBytes),
		ExitCode:      0,
		AccessedFiles: accessed,
	}, nil
}
