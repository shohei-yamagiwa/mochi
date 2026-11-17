package tools

import (
	"context"
	"encoding/json"
	"fmt"
	posixpath "path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
)

const defaultMaxOutputBytes = 32 * 1024

// ExecutionContext provides runtime metadata and the target Sandbox for a tool call.
type ExecutionContext struct {
	AgentRole string
	Sandbox   sandbox.Sandbox
	BaseRef   string
	HeadRef   string
}

// ExecutionResult is the normalized output of executing a sandbox tool.
type ExecutionResult struct {
	Output          string                    `json:"output"`
	ExitCode        int                       `json:"exit_code"`
	AccessedFiles   []domain.FileAccessRecord `json:"accessed_files,omitempty"`
	IsFinalOutput   bool                      `json:"is_final_output,omitempty"`
	SubmittedOutput json.RawMessage           `json:"submitted_output,omitempty"`
}

// Tool is a single executable capability exposed to an LLM agent.
type Tool interface {
	Name() string
	Definition() llm.ToolDefinition
	Execute(ctx context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error)
}

// Registry holds all known tools and produces role-scoped ToolSets.
type Registry struct {
	tools map[string]Tool
}

// RegistryOption configures a Registry.
type RegistryOption func(*Registry) error

// WithCustomTool registers or replaces a tool inside the registry.
func WithCustomTool(tool Tool) RegistryOption {
	return func(r *Registry) error {
		return r.Register(tool)
	}
}

// NewRegistry creates a Registry pre-populated with all built-in sandbox tools.
func NewRegistry(opts ...RegistryOption) (*Registry, error) {
	r := &Registry{
		tools: make(map[string]Tool),
	}

	submitTool, err := NewDefaultSubmitOutputTool()
	if err != nil {
		return nil, fmt.Errorf("init submit_output tool: %w", err)
	}

	builtins := []Tool{
		NewReadFileTool(),
		NewWriteFileTool(),
		NewListDirTool(),
		NewSearchCodeTool(),
		NewGitDiffTool(),
		NewExecCommandTool(),
		submitTool,
	}
	for _, t := range builtins {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}

	for _, opt := range opts {
		if opt != nil {
			if err := opt(r); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

// Register adds a tool to the registry, replacing any existing tool with the same name.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("cannot register nil tool")
	}
	name := strings.TrimSpace(t.Name())
	if name == "" {
		return fmt.Errorf("tool name must not be empty")
	}
	r.tools[name] = t
	return nil
}

// ScopeForRole returns a ToolSet restricted exclusively to allowedTools.
func (r *Registry) ScopeForRole(allowedTools []string) (*ToolSet, error) {
	ordered := make([]Tool, 0, len(allowedTools))
	byName := make(map[string]Tool, len(allowedTools))
	seen := make([]string, 0, len(allowedTools))

	for _, rawName := range allowedTools {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if slices.Contains(seen, name) {
			continue
		}
		tool, ok := r.tools[name]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q in role allowlist", name)
		}
		seen = append(seen, name)
		ordered = append(ordered, tool)
		byName[name] = tool
	}

	return &ToolSet{
		ordered: ordered,
		byName:  byName,
	}, nil
}

// ToolSet is a role-scoped subset of tools ready to pass to an LLM and execute.
type ToolSet struct {
	ordered []Tool
	byName  map[string]Tool
}

// NewToolSet builds a ToolSet directly from an explicit list of tools.
func NewToolSet(tools ...Tool) (*ToolSet, error) {
	ts := &ToolSet{
		ordered: make([]Tool, 0, len(tools)),
		byName:  make(map[string]Tool, len(tools)),
	}
	for _, t := range tools {
		if t == nil {
			return nil, fmt.Errorf("cannot include nil tool in toolset")
		}
		name := strings.TrimSpace(t.Name())
		if name == "" {
			return nil, fmt.Errorf("tool name must not be empty")
		}
		if _, dup := ts.byName[name]; dup {
			return nil, fmt.Errorf("duplicate tool %q in toolset", name)
		}
		ts.ordered = append(ts.ordered, t)
		ts.byName[name] = t
	}
	return ts, nil
}

// Definitions returns the ordered llm.ToolDefinition slice for this role's allowed tools.
func (ts *ToolSet) Definitions() []llm.ToolDefinition {
	if ts == nil {
		return nil
	}
	defs := make([]llm.ToolDefinition, 0, len(ts.ordered))
	for _, t := range ts.ordered {
		defs = append(defs, t.Definition())
	}
	return defs
}

// HasTool reports whether toolName is permitted in this ToolSet.
func (ts *ToolSet) HasTool(toolName string) bool {
	if ts == nil {
		return false
	}
	_, ok := ts.byName[toolName]
	return ok
}

// Execute invokes a tool call if permitted by the role-scoped ToolSet.
// Disallowed tool names and malformed tool errors are surfaced cleanly.
func (ts *ToolSet) Execute(ctx context.Context, execCtx ExecutionContext, call llm.ToolCall) (*ExecutionResult, error) {
	if ts == nil {
		return nil, fmt.Errorf("toolset is nil")
	}
	tool, ok := ts.byName[call.Name]
	if !ok {
		return &ExecutionResult{
			Output:   fmt.Sprintf("error: tool %q is not permitted for role %q", call.Name, execCtx.AgentRole),
			ExitCode: 1,
		}, nil
	}
	return tool.Execute(ctx, execCtx, call.Arguments)
}

func normalizeRepoPath(raw string, allowDot bool) (string, error) {
	p := strings.TrimSpace(strings.ReplaceAll(raw, `\`, "/"))
	if p == "" {
		if allowDot {
			return ".", nil
		}
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q must be repository-relative", raw)
	}
	cleaned := posixpath.Clean(p)
	if cleaned == "." {
		if allowDot {
			return ".", nil
		}
		return "", fmt.Errorf("path %q must reference a file", raw)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes repository root", raw)
	}
	return cleaned, nil
}

// truncateHeadTail caps s to maxBytes using balanced head+tail slices aligned to UTF-8 rune boundaries.
func truncateHeadTail(s string, maxBytes int) string {
	out, _, _, _ := truncateHeadTailWithParts(s, maxBytes)
	return out
}

func truncateHeadTailWithParts(s string, maxBytes int) (out, head, tail string, truncated bool) {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s, s, "", false
	}
	marker := fmt.Sprintf("\n... [truncated %d bytes] ...\n", len(s)-maxBytes)
	if maxBytes <= len(marker)+16 {
		h := s[:trimUTF8End(s, maxBytes)]
		return h, h, "", true
	}
	budget := maxBytes - len(marker)
	headLen := budget / 2
	tailLen := budget - headLen
	headEnd := trimUTF8End(s, headLen)
	tailStart := trimUTF8Start(s, len(s)-tailLen)
	return s[:headEnd] + marker + s[tailStart:], s[:headEnd], s[tailStart:], true
}

func trimUTF8End(s string, maxBytes int) int {
	if maxBytes >= len(s) {
		return len(s)
	}
	if maxBytes <= 0 {
		return 0
	}
	i := maxBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func trimUTF8Start(s string, minStart int) int {
	if minStart <= 0 {
		return 0
	}
	if minStart >= len(s) {
		return len(s)
	}
	i := minStart
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
