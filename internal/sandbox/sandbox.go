package sandbox

import (
	"context"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

// ResourceLimits specifies CPU, memory, and disk caps for a sandbox VM.
type ResourceLimits struct {
	CPU    int `json:"cpu,omitempty"`
	Memory int `json:"memory_gb,omitempty"`
	Disk   int `json:"disk_gb,omitempty"`
}

// CreateOptions configures base sandbox creation and repository preparation.
type CreateOptions struct {
	Name           string
	Workspace      domain.WorkspaceSpec
	EnvVars        map[string]string
	ResourceLimits ResourceLimits
}

// ExecRequest configures a shell command execution inside a sandbox.
type ExecRequest struct {
	Command string
	WorkDir string
	Timeout time.Duration
}

// ExecResult captures command execution output and status.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
}

// FileEntry describes a file or directory returned by ListDir.
type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

// SearchMatch describes a pattern match returned by SearchFiles.
type SearchMatch struct {
	File       string `json:"file"`
	LineNumber int    `json:"line_number"`
	LineText   string `json:"line_text"`
}

// Sandbox abstracts an isolated execution VM (e.g., Daytona sandbox or forked clone).
type Sandbox interface {
	ID() string
	// Fork clones the current Sandbox state into a new isolated Sandbox VM for parallel agents.
	Fork(ctx context.Context, name string) (Sandbox, error)
	// Exec runs a command via daytona.Sandbox.Process.ExecuteCommand.
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	// File & search operations backed by daytona.Sandbox.FileSystem.
	ReadFile(ctx context.Context, path string, startLine, endLine int) (string, error)
	WriteFile(ctx context.Context, path string, content []byte) error
	ListDir(ctx context.Context, path string) ([]FileEntry, error)
	SearchFiles(ctx context.Context, rootPath, pattern string) ([]SearchMatch, error)
	// Destroy tears down the sandbox VM (e.g. daytona.Sandbox.Delete).
	Destroy(ctx context.Context) error
}

// Provider provisions base sandboxes ready for agent execution or forking.
type Provider interface {
	CreateAndPrepareBaseSandbox(ctx context.Context, opts CreateOptions) (Sandbox, error)
}
