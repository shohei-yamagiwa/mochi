package domain

import "context"

// TaskContext is the canonical, platform-neutral input to any workflow.
type TaskContext struct {
	RunID        string         `json:"run_id"`
	WorkflowName string         `json:"workflow_name"`
	TriggerType  string         `json:"trigger_type"`  // e.g. "github_pr", "cli", "webhook"
	DedupKey     string         `json:"dedup_key"`     // e.g. "github:owner/repo:pr:42"
	Workspace    WorkspaceSpec  `json:"workspace"`     // How Daytona should prepare the codebase
	Changeset    *ChangesetSpec `json:"changeset"`     // Structured PR/commit metadata, diff, and changed files
	Variables    map[string]any `json:"variables"`     // Exposed to Go text/template prompts
	SinkMetadata map[string]any `json:"sink_metadata"` // Routing metadata for the OutputSink
}

// WorkspaceSpec describes how the sandbox provider prepares the repository workspace.
type WorkspaceSpec struct {
	RepoURL    string `json:"repo_url"`
	Ref        string `json:"ref"`                  // Head Commit SHA or branch
	BaseRef    string `json:"base_ref"`             // Base branch/SHA for diffs
	CloneToken string `json:"-"`                    // Ephemeral read-only token (never logged or persisted)
	Snapshot   string `json:"snapshot,omitempty"`   // Daytona Snapshot name/ID (fastest ~90ms boot)
	Image      string `json:"image,omitempty"`      // Optional Docker image override
	Dockerfile string `json:"dockerfile,omitempty"` // Optional path to Dockerfile in repo
}

// ChangesetSpec records the exact VCS / Pull Request state at trigger time.
type ChangesetSpec struct {
	Provider       string        `json:"provider"`                  // "github", "git_local", etc.
	RepoOwner      string        `json:"repo_owner"`                // e.g. "shohei-yamagiwa"
	RepoName       string        `json:"repo_name"`                 // e.g. "mochi"
	PRNumber       int           `json:"pr_number,omitempty"`       // Pull Request number (e.g. 42)
	PRURL          string        `json:"pr_url,omitempty"`
	Title          string        `json:"title"`
	Description    string        `json:"description"`
	Author         string        `json:"author"`
	TriggerEvent   string        `json:"trigger_event"`             // e.g. "pull_request.synchronize"
	InstallationID int64         `json:"installation_id,omitempty"` // GitHub App Installation ID
	BaseBranch     string        `json:"base_branch"`               // e.g. "main"
	BaseSHA        string        `json:"base_sha"`                  // Exact base commit SHA
	HeadBranch     string        `json:"head_branch"`               // e.g. "feat/daytona-fork"
	HeadSHA        string        `json:"head_sha"`                  // Exact head commit SHA reviewed
	UnifiedDiff    string        `json:"unified_diff"`              // Full unified diff across the PR
	ChangedFiles   []ChangedFile `json:"changed_files"`             // Per-file patches and line ranges
}

// ChangedFile describes a single modified, added, deleted, or renamed file in a changeset.
type ChangedFile struct {
	Path         string      `json:"path"`
	PreviousPath string      `json:"previous_path,omitempty"` // Set when status == "renamed"
	Status       string      `json:"status"`                  // "added", "modified", "deleted", "renamed"
	Additions    int         `json:"additions"`
	Deletions    int         `json:"deletions"`
	Patch        string      `json:"patch"`         // File-specific unified diff patch
	ChangedHunks []LineRange `json:"changed_hunks"` // New-file line ranges modified in the diff
}

// LineRange represents an inclusive 1-indexed line range [StartLine, EndLine].
type LineRange struct {
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
}

// InputAdapter normalizes an external trigger event into a canonical TaskContext.
type InputAdapter interface {
	Name() string
	BuildTaskContext(ctx context.Context, rawEvent any) (*TaskContext, error)
}
