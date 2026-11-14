package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v93/github"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	ghinput "github.com/shohei-yamagiwa/mochi/internal/input/github"
	"github.com/shohei-yamagiwa/mochi/internal/output"
	ghoutput "github.com/shohei-yamagiwa/mochi/internal/output/github"
)

type capturedReviewReq struct {
	Body     string `json:"body"`
	Event    string `json:"event"`
	CommitID string `json:"commit_id"`
	Comments []struct {
		Path      string `json:"path"`
		Body      string `json:"body"`
		Side      string `json:"side"`
		StartSide string `json:"start_side"`
		Line      int    `json:"line"`
		StartLine int    `json:"start_line"`
	} `json:"comments"`
}

type capturedCheckReq struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Text    string `json:"text"`
	} `json:"output"`
}

func testTaskContext() *domain.TaskContext {
	patch := "@@ -10,4 +10,6 @@ func Verify(tok string) error {\n \tif tok == \"\" {\n+\t\treturn ErrEmpty\n+\t}\n+\tif tok == \"bad\" {\n+\t\treturn ErrBad\n \t}"
	return &domain.TaskContext{
		RunID:        "run-42",
		WorkflowName: "pr-review",
		TriggerType:  "github_pr",
		DedupKey:     "github:shohei-yamagiwa/mochi:pr:42",
		Workspace: domain.WorkspaceSpec{
			RepoURL: "https://github.com/shohei-yamagiwa/mochi.git",
			Ref:     "headsha123456",
			BaseRef: "basesha000000",
		},
		Changeset: &domain.ChangesetSpec{
			Provider:   "github",
			RepoOwner:  "shohei-yamagiwa",
			RepoName:   "mochi",
			PRNumber:   42,
			BaseBranch: "main",
			BaseSHA:    "basesha000000",
			HeadBranch: "fix/auth",
			HeadSHA:    "headsha123456",
			ChangedFiles: []domain.ChangedFile{
				{
					Path:   "internal/auth/service.go",
					Status: "modified",
					Patch:  patch,
				},
			},
		},
	}
}

func TestGitHubSink_FullLifecycleInlineCommentsCoverageAndEnforce(t *testing.T) {
	var (
		mu             sync.Mutex
		createdChecks  []capturedCheckReq
		updatedChecks  []capturedCheckReq
		reviewRequests []capturedReviewReq
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs":
			var req capturedCheckReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode create check run: %v", err)
			}
			createdChecks = append(createdChecks, req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 777, "name": req.Name})

		case r.Method == http.MethodPatch && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs/777":
			var req capturedCheckReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode update check run: %v", err)
			}
			updatedChecks = append(updatedChecks, req)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 777, "status": req.Status})

		case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/42/reviews":
			var req capturedReviewReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode create review: %v", err)
			}
			reviewRequests = append(reviewRequests, req)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 999})

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	baseURL := srv.URL + "/"
	ghClient, err := github.NewClient(github.WithHTTPClient(srv.Client()), github.WithURLs(&baseURL, &baseURL))
	if err != nil {
		t.Fatal(err)
	}

	sink, err := ghoutput.NewSink(ghinput.NewStaticClientFactory(ghClient, ""))
	if err != nil {
		t.Fatal(err)
	}
	configured := sink.WithConfig(domain.SinkConfig{
		Type:                   "github_pr_review",
		PostInlineComments:     true,
		IncludeCoverageSummary: true,
		MinSeverity:            "low",
		MinConfidence:          0.7,
		VerdictMode:            "enforce",
	})

	task := testTaskContext()
	if err := configured.OnRunStarted(context.Background(), task); err != nil {
		t.Fatalf("OnRunStarted: %v", err)
	}

	report := output.ReviewReport{
		Summary: "Found an unvalidated error path and a multi-line branch issue.",
		Verdict: output.VerdictRequestChanges,
		Findings: []output.Finding{
			{
				FilePath:       "internal/auth/service.go",
				StartLine:      11,
				EndLine:        11,
				Severity:       output.SeverityHigh,
				Category:       "correctness",
				Title:          "Return typed auth error",
				Description:    "Include contextual wrapping.",
				SuggestionCode: "\t\treturn fmt.Errorf(\"empty token: %w\", ErrEmpty)",
				Confidence:     0.95,
				SourceAgents:   []string{"correctness"},
			},
			{
				FilePath:     "internal/auth/service.go",
				StartLine:    13,
				EndLine:      15,
				Severity:     output.SeverityMedium,
				Category:     "security",
				Title:        "Constant-time check needed",
				Description:  "Compare token using subtle.ConstantTimeCompare.",
				Confidence:   0.85,
				SourceAgents: []string{"security"},
			},
			{
				FilePath:     "internal/auth/service.go",
				StartLine:    99, // Outside diff hunk -> goes to summary body.
				EndLine:      101,
				Severity:     output.SeverityLow,
				Category:     "architecture",
				Title:        "Out-of-diff helper cleanup",
				Description:  "Consider refactoring legacy helper.",
				Confidence:   0.80,
				SourceAgents: []string{"performance-architecture"},
			},
			{
				FilePath:     "internal/auth/service.go",
				StartLine:    12,
				Severity:     output.SeverityNit, // Filtered by MinSeverity="low"
				Category:     "style",
				Title:        "Minor nit",
				Description:  "Should be omitted.",
				Confidence:   0.99,
				SourceAgents: []string{"correctness"},
			},
			{
				FilePath:     "internal/auth/service.go",
				StartLine:    12,
				Severity:     output.SeverityHigh,
				Category:     "correctness",
				Title:        "Low confidence speculative finding",
				Description:  "Should be omitted.",
				Confidence:   0.40, // Filtered by MinConfidence=0.7
				SourceAgents: []string{"correctness"},
			},
		},
		FailedAgents: []string{"test-runner"},
		Coverage: &domain.CoverageReport{
			RunID:                "run-42",
			PRNumber:             42,
			HeadSHA:              "headsha123456",
			TotalChangedFiles:    2,
			ReviewedChangedFiles: 1,
			FileCoverageRatio:    0.5,
			TotalChangedHunks:    2,
			ReviewedChangedHunks: 1,
			HunkCoverageRatio:    0.5,
			UnreviewedFiles:      []string{"internal/auth/other.go"},
			FileDetails: []domain.FileCoverageDetail{
				{
					Path:              "internal/auth/service.go",
					ChangeStatus:      "modified",
					ChangedHunks:      []domain.LineRange{{StartLine: 10, EndLine: 15}},
					InspectedByAgents: []string{"correctness", "security"},
					FindingsCount:     2,
				},
			},
		},
	}

	rawArtifact, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := configured.Publish(context.Background(), task, rawArtifact); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(createdChecks) != 1 || createdChecks[0].Status != "in_progress" || createdChecks[0].ExternalID != "run-42" {
		t.Fatalf("unexpected created check runs: %+v", createdChecks)
	}
	if len(updatedChecks) != 1 || updatedChecks[0].Status != "completed" || updatedChecks[0].Conclusion != "failure" {
		t.Fatalf("unexpected updated check runs: %+v", updatedChecks)
	}
	if len(reviewRequests) != 1 {
		t.Fatalf("expected 1 PR review request, got %d", len(reviewRequests))
	}

	rev := reviewRequests[0]
	if rev.Event != "REQUEST_CHANGES" {
		t.Fatalf("Event = %q, want REQUEST_CHANGES", rev.Event)
	}
	if len(rev.Comments) != 2 {
		t.Fatalf("expected 2 inline comments, got %d (%+v)", len(rev.Comments), rev.Comments)
	}

	// Comment 0: single-line comment MUST NOT set StartLine / StartSide.
	c0 := rev.Comments[0]
	if c0.Line != 11 || c0.Side != "RIGHT" || c0.StartLine != 0 || c0.StartSide != "" {
		t.Fatalf("unexpected single-line comment fields: %+v", c0)
	}
	if !strings.Contains(c0.Body, "```suggestion\n\t\treturn fmt.Errorf") {
		t.Fatalf("expected suggestion block in c0 body, got %q", c0.Body)
	}

	// Comment 1: multi-line comment MUST set StartLine, StartSide, Line, Side.
	c1 := rev.Comments[1]
	if c1.StartLine != 13 || c1.Line != 15 || c1.StartSide != "RIGHT" || c1.Side != "RIGHT" {
		t.Fatalf("unexpected multi-line comment fields: %+v", c1)
	}

	// Out-of-diff finding, filtered count note, failed agent notice, and coverage <details> belong in summary body.
	for _, want := range []string{
		"Out-of-diff helper cleanup",
		"2 additional finding(s) omitted",
		"Partial Review Notice",
		"test-runner",
		"<details>",
		"Review Coverage (1/2 files, 50% hunks inspected at commit headsha)",
		"internal/auth/other.go",
	} {
		if !strings.Contains(rev.Body, want) {
			t.Fatalf("review body missing %q:\n%s", want, rev.Body)
		}
	}
	if strings.Contains(rev.Body, "Minor nit") || strings.Contains(rev.Body, "Low confidence speculative finding") {
		t.Fatalf("filtered findings leaked into review body:\n%s", rev.Body)
	}
}

func TestGitHubSink_HTTP422InlineRetryAndCommentOnlyAndOnRunFailed(t *testing.T) {
	var (
		mu             sync.Mutex
		reviewAttempts int
		lastReview     capturedReviewReq
		updatedChecks  []capturedCheckReq
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 888})

		case r.Method == http.MethodPatch && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs/888":
			var req capturedCheckReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			updatedChecks = append(updatedChecks, req)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 888})

		case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/42/reviews":
			reviewAttempts++
			var req capturedReviewReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			lastReview = req
			if reviewAttempts == 1 && len(req.Comments) > 0 {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": "Line could not be resolved",
				})
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001})

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	baseURL := srv.URL + "/"
	ghClient, err := github.NewClient(github.WithHTTPClient(srv.Client()), github.WithURLs(&baseURL, &baseURL))
	if err != nil {
		t.Fatal(err)
	}

	sink, err := ghoutput.NewSink(ghinput.NewStaticClientFactory(ghClient, ""))
	if err != nil {
		t.Fatal(err)
	}
	configured := sink.WithConfig(domain.SinkConfig{
		Type:                   "github_pr_review",
		PostInlineComments:     true,
		IncludeCoverageSummary: false,
		VerdictMode:            "comment_only",
	})

	task := testTaskContext()
	if err := configured.OnRunStarted(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	report := output.ReviewReport{
		Summary: "Needs changes",
		Verdict: output.VerdictRequestChanges,
		Findings: []output.Finding{
			{
				FilePath:     "internal/auth/service.go",
				StartLine:    11,
				Severity:     output.SeverityHigh,
				Category:     "correctness",
				Title:        "Line moved by concurrent push",
				Description:  "Should appear in fallback summary body.",
				Confidence:   0.9,
				SourceAgents: []string{"correctness"},
			},
		},
	}
	raw, _ := json.Marshal(report)
	if err := configured.Publish(context.Background(), task, raw); err != nil {
		t.Fatalf("Publish with 422 fallback failed: %v", err)
	}

	mu.Lock()
	if reviewAttempts != 2 {
		mu.Unlock()
		t.Fatalf("reviewAttempts = %d, want 2", reviewAttempts)
	}
	if lastReview.Event != "COMMENT" {
		mu.Unlock()
		t.Fatalf("comment_only mode Event = %q, want COMMENT", lastReview.Event)
	}
	if len(lastReview.Comments) != 0 {
		mu.Unlock()
		t.Fatalf("fallback review should have 0 inline comments, got %d", len(lastReview.Comments))
	}
	if !strings.Contains(lastReview.Body, "Line moved by concurrent push") {
		mu.Unlock()
		t.Fatalf("fallback review body missing inline finding:\n%s", lastReview.Body)
	}
	mu.Unlock()

	// Now verify OnRunFailed updates check run with cancelled on context.Canceled.
	task2 := testTaskContext()
	task2.RunID = "run-cancelled"
	if err := configured.OnRunStarted(context.Background(), task2); err != nil {
		t.Fatal(err)
	}
	if err := configured.OnRunFailed(context.Background(), task2, errors.Join(context.Canceled, errors.New("superseded"))); err != nil {
		t.Fatalf("OnRunFailed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updatedChecks) != 2 {
		t.Fatalf("expected 2 updated check runs, got %d", len(updatedChecks))
	}
	if updatedChecks[1].Conclusion != "cancelled" {
		t.Fatalf("OnRunFailed Conclusion = %q, want cancelled", updatedChecks[1].Conclusion)
	}
}
