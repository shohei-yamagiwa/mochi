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

type readFileArgs struct {
	Path      string `json:"path" jsonschema:"Repository-relative path of the file to read"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"Optional 1-indexed start line"`
	EndLine   int    `json:"end_line,omitempty" jsonschema:"Optional 1-indexed end line"`
}

type readFileTool struct {
	schema   *jsonschema.Schema
	maxBytes int
}

// NewReadFileTool creates the read_file sandbox tool.
func NewReadFileTool() Tool {
	s, _ := jsonschema.For[readFileArgs](nil)
	return &readFileTool{
		schema:   s,
		maxBytes: defaultMaxOutputBytes,
	}
}

func (t *readFileTool) Name() string { return "read_file" }

func (t *readFileTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.Name(),
		Description: "Read a file from the sandbox repository, optionally restricted to a 1-indexed [start_line, end_line] range.",
		Parameters:  t.schema,
	}
}

func (t *readFileTool) Execute(ctx context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error) {
	if execCtx.Sandbox == nil {
		return nil, fmt.Errorf("read_file: sandbox is nil")
	}
	var args readFileArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("invalid arguments: %v", err),
			ExitCode: 1,
		}, nil
	}
	cleanPath, err := normalizeRepoPath(args.Path, false)
	if err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("invalid path: %v", err),
			ExitCode: 1,
		}, nil
	}

	content, err := execCtx.Sandbox.ReadFile(ctx, cleanPath, args.StartLine, args.EndLine)
	if err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("read_file error: %v", err),
			ExitCode: 1,
		}, nil
	}

	startLine := args.StartLine
	endLine := args.EndLine
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 {
		lineCount := countLines(content)
		if lineCount == 0 {
			endLine = startLine
		} else {
			endLine = startLine + lineCount - 1
		}
	}

	out, head, tail, truncated := truncateHeadTailWithParts(content, t.maxBytes)
	var accessed []domain.FileAccessRecord
	if !truncated {
		accessed = []domain.FileAccessRecord{
			{
				AgentRole: execCtx.AgentRole,
				Path:      cleanPath,
				Kind:      domain.FileAccessRead,
				LineRange: &domain.LineRange{
					StartLine: startLine,
					EndLine:   endLine,
				},
			},
		}
	} else {
		headLines := strings.Count(head, "\n")
		if headLines == 0 && len(head) > 0 {
			headLines = 1
		}
		if headLines > 0 {
			headEnd := min(endLine, startLine+headLines-1)
			accessed = append(accessed, domain.FileAccessRecord{
				AgentRole: execCtx.AgentRole,
				Path:      cleanPath,
				Kind:      domain.FileAccessRead,
				LineRange: &domain.LineRange{
					StartLine: startLine,
					EndLine:   headEnd,
				},
			})
		}
		tailLines := strings.Count(strings.TrimSuffix(tail, "\n"), "\n")
		if tailLines == 0 && len(tail) > 0 {
			tailLines = 1
		}
		if tailLines > 0 {
			tailStart := max(startLine, endLine-tailLines+1)
			accessed = append(accessed, domain.FileAccessRecord{
				AgentRole: execCtx.AgentRole,
				Path:      cleanPath,
				Kind:      domain.FileAccessRead,
				LineRange: &domain.LineRange{
					StartLine: tailStart,
					EndLine:   endLine,
				},
			})
		}
	}

	return &ExecutionResult{
		Output:        out,
		ExitCode:      0,
		AccessedFiles: accessed,
	}, nil
}

type writeFileArgs struct {
	Path    string `json:"path" jsonschema:"Repository-relative path of the file to write"`
	Content string `json:"content" jsonschema:"Full file contents to write"`
}

type writeFileTool struct {
	schema *jsonschema.Schema
}

// NewWriteFileTool creates the write_file sandbox tool.
func NewWriteFileTool() Tool {
	s, _ := jsonschema.For[writeFileArgs](nil)
	return &writeFileTool{schema: s}
}

func (t *writeFileTool) Name() string { return "write_file" }

func (t *writeFileTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.Name(),
		Description: "Write or overwrite a file in the sandbox repository.",
		Parameters:  t.schema,
	}
}

func (t *writeFileTool) Execute(ctx context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error) {
	if execCtx.Sandbox == nil {
		return nil, fmt.Errorf("write_file: sandbox is nil")
	}
	var args writeFileArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("invalid arguments: %v", err),
			ExitCode: 1,
		}, nil
	}
	cleanPath, err := normalizeRepoPath(args.Path, false)
	if err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("invalid path: %v", err),
			ExitCode: 1,
		}, nil
	}

	if err := execCtx.Sandbox.WriteFile(ctx, cleanPath, []byte(args.Content)); err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("write_file error: %v", err),
			ExitCode: 1,
		}, nil
	}

	return &ExecutionResult{
		Output:   fmt.Sprintf("wrote %d bytes to %s", len(args.Content), cleanPath),
		ExitCode: 0,
		AccessedFiles: []domain.FileAccessRecord{
			{
				AgentRole: execCtx.AgentRole,
				Path:      cleanPath,
				Kind:      domain.FileAccessWrite,
			},
		},
	}, nil
}

type listDirArgs struct {
	Path string `json:"path,omitempty" jsonschema:"Optional repository-relative directory path (defaults to '.')"`
}

type listDirTool struct {
	schema *jsonschema.Schema
}

// NewListDirTool creates the list_dir sandbox tool.
func NewListDirTool() Tool {
	s, _ := jsonschema.For[listDirArgs](nil)
	return &listDirTool{schema: s}
}

func (t *listDirTool) Name() string { return "list_dir" }

func (t *listDirTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.Name(),
		Description: "List files and directories inside a repository directory in the sandbox.",
		Parameters:  t.schema,
	}
}

func (t *listDirTool) Execute(ctx context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error) {
	if execCtx.Sandbox == nil {
		return nil, fmt.Errorf("list_dir: sandbox is nil")
	}
	var args listDirArgs
	if len(strings.TrimSpace(string(rawArgs))) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return &ExecutionResult{
				Output:   fmt.Sprintf("invalid arguments: %v", err),
				ExitCode: 1,
			}, nil
		}
	}
	cleanPath, err := normalizeRepoPath(args.Path, true)
	if err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("invalid path: %v", err),
			ExitCode: 1,
		}, nil
	}

	entries, err := execCtx.Sandbox.ListDir(ctx, cleanPath)
	if err != nil {
		return &ExecutionResult{
			Output:   fmt.Sprintf("list_dir error: %v", err),
			ExitCode: 1,
		}, nil
	}

	encoded, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("list_dir marshal entries: %w", err)
	}

	return &ExecutionResult{
		Output:   truncateHeadTail(string(encoded), defaultMaxOutputBytes),
		ExitCode: 0,
		AccessedFiles: []domain.FileAccessRecord{
			{
				AgentRole: execCtx.AgentRole,
				Path:      cleanPath,
				Kind:      domain.FileAccessList,
			},
		},
	}, nil
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n") + 1
	if strings.HasSuffix(s, "\n") {
		n--
	}
	if n < 1 {
		return 1
	}
	return n
}
