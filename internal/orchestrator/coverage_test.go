package orchestrator

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/output"
)

func TestComputeCoverageReport_MultiAgentAndHunkCoverage(t *testing.T) {
	changeset := &domain.ChangesetSpec{
		RepoOwner: "shohei-yamagiwa",
		RepoName:  "mochi",
		PRNumber:  42,
		BaseSHA:   "1111111111111111",
		HeadSHA:   "2222222222222222",
		ChangedFiles: []domain.ChangedFile{
			{
				Path:   "internal/auth/service.go",
				Status: "modified",
				ChangedHunks: []domain.LineRange{
					{StartLine: 10, EndLine: 25},
					{StartLine: 80, EndLine: 95},
				},
			},
			{
				Path:         "internal/api/router.go",
				PreviousPath: "internal/api/routes.go",
				Status:       "renamed",
				ChangedHunks: []domain.LineRange{
					{StartLine: 30, EndLine: 45},
				},
			},
			{
				Path:   "internal/db/query.go",
				Status: "modified",
				ChangedHunks: []domain.LineRange{
					{StartLine: 5, EndLine: 15},
				},
			},
			{
				Path:   "internal/legacy/old.go",
				Status: "deleted",
			},
			{
				Path:   "docs/README.md",
				Status: "modified",
				ChangedHunks: []domain.LineRange{
					{StartLine: 1, EndLine: 10},
				},
			},
		},
	}

	toolExecs := []*domain.ToolExecRecord{
		{
			ToolName: "read_file",
			ExitCode: 0,
			AccessedFiles: []domain.FileAccessRecord{
				{
					Path:      "internal/auth/service.go",
					LineRange: &domain.LineRange{StartLine: 1, EndLine: 30},
					AgentRole: "correctness",
					Kind:      domain.FileAccessRead,
				},
				{
					Path:      "internal/auth/service.go",
					LineRange: &domain.LineRange{StartLine: 25, EndLine: 40},
					AgentRole: "correctness",
					Kind:      domain.FileAccessRead,
				},
			},
		},
		{
			ToolName: "search_code",
			ExitCode: 0,
			AccessedFiles: []domain.FileAccessRecord{
				{
					Path:      "internal/api/routes.go", // accessed via PreviousPath
					LineRange: &domain.LineRange{StartLine: 35, EndLine: 35},
					AgentRole: "security",
					Kind:      domain.FileAccessSearch,
				},
				{
					Path:      "internal/config/config.go", // out-of-diff context file
					LineRange: &domain.LineRange{StartLine: 12, EndLine: 12},
					AgentRole: "security",
					Kind:      domain.FileAccessSearch,
				},
			},
		},
		{
			ToolName: "git_diff",
			ExitCode: 0,
			AccessedFiles: []domain.FileAccessRecord{
				{
					Path:      "internal/db", // directory prefix diff
					AgentRole: "performance_architecture",
					Kind:      domain.FileAccessDiff,
				},
				{
					Path:      "internal/legacy/old.go",
					AgentRole: "correctness",
					Kind:      domain.FileAccessDiff,
				},
			},
		},
		{
			ToolName: "submit_output",
			ExitCode: 0,
			AccessedFiles: []domain.FileAccessRecord{
				{
					Path:      "internal/auth/service.go",
					LineRange: &domain.LineRange{StartLine: 15, EndLine: 20},
					AgentRole: "security",
					Kind:      domain.FileAccessCite,
				},
			},
		},
		{
			// Failed tool execution should be ignored.
			ToolName: "read_file",
			ExitCode: 1,
			AccessedFiles: []domain.FileAccessRecord{
				{
					Path:      "docs/README.md",
					LineRange: &domain.LineRange{StartLine: 1, EndLine: 50},
					AgentRole: "correctness",
					Kind:      domain.FileAccessRead,
				},
			},
		},
		{
			ToolName: "read_file",
			ExitCode: 0,
			AccessedFiles: []domain.FileAccessRecord{
				{
					Path:      "./pkg/helper/util.go", // out-of-diff context file
					LineRange: &domain.LineRange{StartLine: 1, EndLine: 20},
					AgentRole: "correctness",
					Kind:      domain.FileAccessRead,
				},
			},
		},
	}

	report := ComputeCoverageReport("run-123", changeset, toolExecs)

	if report.RunID != "run-123" {
		t.Fatalf("RunID = %q, want run-123", report.RunID)
	}
	if report.TotalChangedFiles != 5 {
		t.Fatalf("TotalChangedFiles = %d, want 5", report.TotalChangedFiles)
	}
	if report.ReviewedChangedFiles != 4 {
		t.Fatalf("ReviewedChangedFiles = %d, want 4", report.ReviewedChangedFiles)
	}
	if report.FileCoverageRatio != 0.8 {
		t.Fatalf("FileCoverageRatio = %v, want 0.8", report.FileCoverageRatio)
	}

	// Total hunks: 2 (auth) + 1 (router) + 1 (db) + 0 (deleted) + 1 (docs) = 5
	// Reviewed hunks: 1 (auth first hunk; second hunk 80-95 uncovered) + 1 (router) + 1 (db) + 0 (docs) = 3
	if report.TotalChangedHunks != 5 {
		t.Fatalf("TotalChangedHunks = %d, want 5", report.TotalChangedHunks)
	}
	if report.ReviewedChangedHunks != 3 {
		t.Fatalf("ReviewedChangedHunks = %d, want 3", report.ReviewedChangedHunks)
	}
	if report.HunkCoverageRatio != 0.6 {
		t.Fatalf("HunkCoverageRatio = %v, want 0.6", report.HunkCoverageRatio)
	}

	if !slices.Equal(report.UnreviewedFiles, []string{"docs/README.md"}) {
		t.Fatalf("UnreviewedFiles = %v, want [docs/README.md]", report.UnreviewedFiles)
	}

	wantContext := []string{"internal/config/config.go", "pkg/helper/util.go"}
	if !slices.Equal(report.ContextFilesRead, wantContext) {
		t.Fatalf("ContextFilesRead = %v, want %v", report.ContextFilesRead, wantContext)
	}

	// Verify auth/service.go details: merged read ranges [1..40], uncovered hunk [80..95], findings count 1, agents [correctness, security].
	authDetail := report.FileDetails[0]
	if authDetail.Path != "internal/auth/service.go" {
		t.Fatalf("FileDetails[0].Path = %q, want internal/auth/service.go", authDetail.Path)
	}
	if authDetail.FindingsCount != 1 {
		t.Fatalf("authDetail.FindingsCount = %d, want 1", authDetail.FindingsCount)
	}
	if !slices.Equal(authDetail.InspectedByAgents, []string{"correctness", "security"}) {
		t.Fatalf("authDetail.InspectedByAgents = %v, want [correctness security]", authDetail.InspectedByAgents)
	}
	mergedRanges := authDetail.ReadRangesByAgent["correctness"]
	if len(mergedRanges) != 1 || mergedRanges[0].StartLine != 1 || mergedRanges[0].EndLine != 40 {
		t.Fatalf("authDetail.ReadRangesByAgent[correctness] = %+v, want [{1 40}]", mergedRanges)
	}
	if len(authDetail.UncoveredHunks) != 1 || authDetail.UncoveredHunks[0].StartLine != 80 || authDetail.UncoveredHunks[0].EndLine != 95 {
		t.Fatalf("authDetail.UncoveredHunks = %+v, want [{80 95}]", authDetail.UncoveredHunks)
	}

	// Verify renamed router.go matched via PreviousPath.
	routerDetail := report.FileDetails[1]
	if !slices.Equal(routerDetail.InspectedByAgents, []string{"security"}) {
		t.Fatalf("routerDetail.InspectedByAgents = %v, want [security]", routerDetail.InspectedByAgents)
	}
	if len(routerDetail.UncoveredHunks) != 0 {
		t.Fatalf("routerDetail.UncoveredHunks = %+v, want empty", routerDetail.UncoveredHunks)
	}
}

func TestComputeCoverageReport_FallbackPatchAndUnifiedDiffParsing(t *testing.T) {
	changeset := &domain.ChangesetSpec{
		RepoOwner: "shohei-yamagiwa",
		RepoName:  "mochi",
		PRNumber:  7,
		BaseSHA:   "aaaa",
		HeadSHA:   "bbbb",
		UnifiedDiff: `diff --git a/pkg/calc/add.go b/pkg/calc/add.go
index 1111111..2222222 100644
--- a/pkg/calc/add.go
+++ b/pkg/calc/add.go
@@ -10,3 +12,6 @@ func Add(a, b int) int {
+	// comment
@@ -50 +55,2 @@ func Sub(a, b int) int {
+	return a - b
`,
		ChangedFiles: []domain.ChangedFile{
			{
				Path:   "pkg/calc/mul.go",
				Status: "modified",
				Patch: `@@ -1,4 +1,8 @@
+package calc
@@ -20,2 +25 @@
+func Mul() {}`,
			},
			{
				Path:   "pkg/calc/add.go",
				Status: "modified",
				// No ChangedHunks and no Patch; should fall back to UnifiedDiff.
			},
		},
	}

	toolExecs := []*domain.ToolExecRecord{
		{
			ToolName: "read_file",
			ExitCode: 0,
			AccessedFiles: []domain.FileAccessRecord{
				{
					Path:      "pkg/calc/mul.go",
					LineRange: &domain.LineRange{StartLine: 1, EndLine: 10},
					AgentRole: "correctness",
					Kind:      domain.FileAccessRead,
				},
				{
					Path:      "pkg/calc/add.go",
					LineRange: &domain.LineRange{StartLine: 1, EndLine: 100},
					AgentRole: "test_runner",
					Kind:      domain.FileAccessRead,
				},
			},
		},
	}

	report := ComputeCoverageReport("run-fallback", changeset, toolExecs)

	if len(report.FileDetails) != 2 {
		t.Fatalf("len(FileDetails) = %d, want 2", len(report.FileDetails))
	}

	mulHunks := report.FileDetails[0].ChangedHunks
	wantMulHunks := []domain.LineRange{{StartLine: 1, EndLine: 8}, {StartLine: 25, EndLine: 25}}
	if !slices.Equal(mulHunks, wantMulHunks) {
		t.Fatalf("mul.go ChangedHunks = %+v, want %+v", mulHunks, wantMulHunks)
	}
	if len(report.FileDetails[0].UncoveredHunks) != 1 || report.FileDetails[0].UncoveredHunks[0].StartLine != 25 {
		t.Fatalf("mul.go UncoveredHunks = %+v, want [{25 25}]", report.FileDetails[0].UncoveredHunks)
	}

	addHunks := report.FileDetails[1].ChangedHunks
	wantAddHunks := []domain.LineRange{{StartLine: 12, EndLine: 17}, {StartLine: 55, EndLine: 56}}
	if !slices.Equal(addHunks, wantAddHunks) {
		t.Fatalf("add.go ChangedHunks = %+v, want %+v", addHunks, wantAddHunks)
	}
	if len(report.FileDetails[1].UncoveredHunks) != 0 {
		t.Fatalf("add.go UncoveredHunks = %+v, want empty", report.FileDetails[1].UncoveredHunks)
	}
}

func TestComputeCoverageReport_ValidatesInReviewReportSchema(t *testing.T) {
	validator, err := output.NewReviewReportValidator()
	if err != nil {
		t.Fatalf("NewReviewReportValidator() error = %v", err)
	}

	changeset := &domain.ChangesetSpec{
		RepoOwner: "shohei-yamagiwa",
		RepoName:  "mochi",
		PRNumber:  99,
		BaseSHA:   "base",
		HeadSHA:   "head",
		ChangedFiles: []domain.ChangedFile{
			{
				Path:         "main.go",
				Status:       "modified",
				ChangedHunks: []domain.LineRange{{StartLine: 1, EndLine: 5}},
			},
		},
	}

	cov := ComputeCoverageReport("run-schema", changeset, nil)
	rr := output.ReviewReport{
		Summary:  "All good",
		Verdict:  output.VerdictApprove,
		Findings: []output.Finding{},
		Coverage: cov,
	}

	raw, err := json.Marshal(rr)
	if err != nil {
		t.Fatalf("json.Marshal(ReviewReport) error = %v", err)
	}
	if _, err := validator.ValidateJSON(raw); err != nil {
		t.Fatalf("ValidateJSON(ReviewReport) error = %v\nJSON: %s", err, string(raw))
	}
}
