package output_test

import (
	"encoding/json"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/output"
)

func TestReviewReportValidator(t *testing.T) {
	v, err := output.NewReviewReportValidator()
	if err != nil {
		t.Fatalf("NewReviewReportValidator failed: %v", err)
	}

	valid := output.ReviewReport{
		Summary: "Found one nil pointer issue and all tests pass.",
		Verdict: output.VerdictRequestChanges,
		Findings: []output.Finding{
			{
				FilePath:       "internal/foo/bar.go",
				StartLine:      42,
				EndLine:        45,
				Severity:       output.SeverityHigh,
				Category:       "correctness",
				Title:          "Nil dereference on empty response",
				Description:    "resp may be nil when err is non-nil.",
				SuggestionCode: "if err != nil {\n\treturn err\n}",
				Confidence:     0.92,
				SourceAgents:   []string{"correctness"},
			},
		},
		TestExecution: &output.TestSummary{
			Ran:      true,
			Passed:   true,
			Commands: []string{"go test ./..."},
			Summary:  "All tests passed",
		},
		Coverage: &domain.CoverageReport{
			RunID:                "run-1",
			BaseSHA:              "base123",
			HeadSHA:              "head456",
			TotalChangedFiles:    1,
			ReviewedChangedFiles: 1,
			FileCoverageRatio:    1.0,
			TotalChangedHunks:    1,
			ReviewedChangedHunks: 1,
			HunkCoverageRatio:    1.0,
			UnreviewedFiles:      []string{},
			FileDetails:          []domain.FileCoverageDetail{},
			ContextFilesRead:     []string{},
		},
		AgentSummaries: map[string]string{
			"correctness": "1 high-severity finding",
		},
	}

	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("marshal valid report: %v", err)
	}
	parsed, err := v.ValidateJSON(raw)
	if err != nil {
		t.Fatalf("ValidateJSON unexpectedly failed on valid payload: %v", err)
	}
	if parsed.Verdict != output.VerdictRequestChanges || len(parsed.Findings) != 1 {
		t.Fatalf("unexpected parsed report: %+v", parsed)
	}

	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "invalid verdict",
			payload: `{"summary":"ok","verdict":"SHIP_IT","findings":[]}`,
		},
		{
			name:    "invalid severity",
			payload: `{"summary":"ok","verdict":"COMMENT","findings":[{"file_path":"a.go","severity":"urgent","category":"security","title":"t","description":"d","confidence":0.8,"source_agents":["security"]}]}`,
		},
		{
			name:    "confidence out of range",
			payload: `{"summary":"ok","verdict":"COMMENT","findings":[{"file_path":"a.go","severity":"low","category":"security","title":"t","description":"d","confidence":1.5,"source_agents":["security"]}]}`,
		},
		{
			name:    "missing required summary",
			payload: `{"verdict":"APPROVE","findings":[]}`,
		},
		{
			name:    "unknown field rejected",
			payload: `{"summary":"ok","verdict":"APPROVE","findings":[],"extra_field":true}`,
		},
		{
			name:    "trailing garbage rejected",
			payload: `{"summary":"ok","verdict":"APPROVE","findings":[]}{"extra":1}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.ValidateJSON([]byte(tc.payload)); err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}
}
