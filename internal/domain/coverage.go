package domain

import "time"

// FileAccessKind identifies how a sandbox tool inspected or cited a repository path.
type FileAccessKind string

const (
	FileAccessRead   FileAccessKind = "read"
	FileAccessDiff   FileAccessKind = "diff"
	FileAccessSearch FileAccessKind = "search"
	FileAccessCite   FileAccessKind = "cite"
	FileAccessList   FileAccessKind = "list"
	FileAccessWrite  FileAccessKind = "write"
)

// FileAccessRecord captures structured file/line-range access emitted by sandbox tools.
type FileAccessRecord struct {
	AgentRole string         `json:"agent_role"`
	Path      string         `json:"path"`
	Kind      FileAccessKind `json:"kind"`
	LineRange *LineRange     `json:"line_range,omitempty"`
}

// CoverageReport measures the gap between PR changed files/hunks and what agents inspected.
type CoverageReport struct {
	RunID                string               `json:"run_id"`
	PRNumber             int                  `json:"pr_number,omitempty"`
	BaseSHA              string               `json:"base_sha"`
	HeadSHA              string               `json:"head_sha"`
	TotalChangedFiles    int                  `json:"total_changed_files"`
	ReviewedChangedFiles int                  `json:"reviewed_changed_files"`
	FileCoverageRatio    float64              `json:"file_coverage_ratio"` // 0.0 - 1.0
	TotalChangedHunks    int                  `json:"total_changed_hunks"`
	ReviewedChangedHunks int                  `json:"reviewed_changed_hunks"`
	HunkCoverageRatio    float64              `json:"hunk_coverage_ratio"` // 0.0 - 1.0
	UnreviewedFiles      []string             `json:"unreviewed_files"`    // Changed in PR diff, but no agent inspected them
	FileDetails          []FileCoverageDetail `json:"file_details"`        // Per-file breakdown
	ContextFilesRead     []string             `json:"context_files_read"`  // Unchanged files read by agents for surrounding context
	CreatedAt            time.Time            `json:"created_at"`
}

// FileCoverageDetail breaks down inspection coverage for a single changed file.
type FileCoverageDetail struct {
	Path              string                 `json:"path"`
	ChangeStatus      string                 `json:"change_status"`        // "added", "modified", "deleted", "renamed"
	ChangedHunks      []LineRange            `json:"changed_hunks"`        // Line ranges modified in PR
	ReadRangesByAgent map[string][]LineRange `json:"read_ranges_by_agent"` // Role -> line ranges read
	InspectedByAgents []string               `json:"inspected_by_agents"`  // Which specialist roles examined this file
	UncoveredHunks    []LineRange            `json:"uncovered_hunks"`      // Changed hunks not overlapped by any read_file / diff
	FindingsCount     int                    `json:"findings_count"`
}

// FullyReviewed reports whether every changed file and hunk was inspected
// by at least one agent during the run.
func (r CoverageReport) FullyReviewed() bool {
	return r.FileCoverageRatio >= 1.0 && r.HunkCoverageRatio >= 1.0
}
