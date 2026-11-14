package console_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/output"
	"github.com/shohei-yamagiwa/mochi/internal/output/console"
)

func TestConsoleSink_PublishAndOnRunFailed(t *testing.T) {
	var buf bytes.Buffer
	sink := console.NewSink(&buf).WithConfig(domain.SinkConfig{
		Type:                   "console",
		IncludeCoverageSummary: true,
		MinSeverity:            "medium",
		MinConfidence:          0.6,
	})

	task := &domain.TaskContext{
		RunID:        "run-1",
		WorkflowName: "pr-review",
		Changeset: &domain.ChangesetSpec{
			RepoOwner: "octo",
			RepoName:  "repo",
			PRNumber:  9,
			Title:     "Optimize query",
		},
	}

	rep := output.ReviewReport{
		Summary: "One solid finding.",
		Verdict: output.VerdictComment,
		Findings: []output.Finding{
			{
				FilePath:     "db/query.go",
				StartLine:    42,
				Severity:     output.SeverityHigh,
				Category:     "performance",
				Title:        "Missing index on tenant_id",
				Description:  "Causes full table scan.",
				Confidence:   0.92,
				SourceAgents: []string{"performance-architecture"},
			},
			{
				FilePath:     "db/query.go",
				StartLine:    10,
				Severity:     output.SeverityNit,
				Category:     "style",
				Title:        "Rename variable",
				Description:  "Nit only.",
				Confidence:   0.95,
				SourceAgents: []string{"correctness"},
			},
		},
		Coverage: &domain.CoverageReport{
			TotalChangedFiles:    1,
			ReviewedChangedFiles: 1,
			HunkCoverageRatio:    1.0,
			FileDetails: []domain.FileCoverageDetail{
				{
					Path:              "db/query.go",
					ChangeStatus:      "modified",
					ChangedHunks:      []domain.LineRange{{StartLine: 40, EndLine: 45}},
					InspectedByAgents: []string{"performance-architecture"},
					FindingsCount:     1,
				},
			},
		},
	}
	raw, _ := json.Marshal(rep)
	if err := sink.Publish(context.Background(), task, raw); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"# Mochi Review Report — `COMMENT`",
		"Missing index on tenant_id",
		"1 finding(s) filtered",
		"Review Coverage (1/1 files, 100% hunks)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("console output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Rename variable") {
		t.Fatalf("nit finding should be filtered out:\n%s", out)
	}

	buf.Reset()
	if err := sink.OnRunFailed(context.Background(), task, errors.New("boom")); err != nil {
		t.Fatalf("OnRunFailed: %v", err)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Fatalf("expected failure message in output: %q", buf.String())
	}
}
