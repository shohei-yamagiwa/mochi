package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

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
	if lastReview.Event != "REQUEST_CHANGES" {
		mu.Unlock()
		t.Fatalf("Event = %q, want REQUEST_CHANGES", lastReview.Event)
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

func TestGitHubSink_LargeReviewBodyCapAndTrackerCleanup(t *testing.T) {
	var (
		mu             sync.Mutex
		reviewAttempts int
		reviewBodies   []string
		checkUpdates   int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 909})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs/909":
			checkUpdates++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 909})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/42/reviews":
			reviewAttempts++
			var req capturedReviewReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			reviewBodies = append(reviewBodies, req.Body)
			if reviewAttempts == 1 {
				// Force 422 inline fallback to also test fallback body capping.
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "stale diff"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2001})
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
	})

	fileDetails := make([]domain.FileCoverageDetail, 0, 1200)
	unreviewed := make([]string, 0, 1200)
	for i := range 1200 {
		p := fmt.Sprintf("pkg/service/module_%04d/handler_implementation.go", i)
		fileDetails = append(fileDetails, domain.FileCoverageDetail{
			Path:              p,
			ChangeStatus:      "modified",
			ChangedHunks:      []domain.LineRange{{StartLine: 1, EndLine: 5}},
			InspectedByAgents: []string{"correctness"},
		})
		unreviewed = append(unreviewed, p)
	}

	// Include multi-byte UTF-8 text in summary to ensure truncation never breaks runes.
	multiByteSummary := strings.Repeat("レビュー結果の確認が必要です。", 3000) // ~117 KB UTF-8

	task := testTaskContext()
	task.RunID = "run-large-1"
	if err := configured.OnRunStarted(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	report := output.ReviewReport{
		Summary: multiByteSummary,
		Verdict: output.VerdictComment,
		Findings: []output.Finding{
			{
				FilePath:     "internal/auth/service.go",
				StartLine:    11,
				Severity:     output.SeverityHigh,
				Category:     "correctness",
				Title:        "Inline finding merged in 422 fallback",
				Description:  strings.Repeat("日本語説明テスト", 200),
				Confidence:   0.95,
				SourceAgents: []string{"correctness"},
			},
		},
		Coverage: &domain.CoverageReport{
			TotalChangedFiles:    1200,
			ReviewedChangedFiles: 0,
			HunkCoverageRatio:    0,
			UnreviewedFiles:      unreviewed,
			FileDetails:          fileDetails,
		},
	}
	raw, _ := json.Marshal(report)
	if err := configured.Publish(context.Background(), task, raw); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Calling OnRunFailed after Publish completed must be a no-op because tracker deleted the entry.
	if err := configured.OnRunFailed(context.Background(), task, errors.New("late error")); err != nil {
		t.Fatalf("OnRunFailed after Publish: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reviewBodies) != 2 {
		t.Fatalf("expected 2 review attempts (primary + 422 fallback), got %d", len(reviewBodies))
	}
	for i, body := range reviewBodies {
		if len(body) > 65536 {
			t.Fatalf("reviewBodies[%d] length %d exceeds GitHub's 65536 limit", i, len(body))
		}
		if !utf8.ValidString(body) {
			t.Fatalf("reviewBodies[%d] contains invalid UTF-8 after truncation", i)
		}
	}
	if checkUpdates != 1 {
		t.Fatalf("expected exactly 1 check run update (tracker cleaned up), got %d", checkUpdates)
	}
}

func TestGitHubSink_CompactedCheckRunNoSelfReferenceAndBalancedTruncation(t *testing.T) {
	var (
		mu           sync.Mutex
		reviewBodies []string
		checkUpdates []capturedCheckReq
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 910})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs/910":
			var req capturedCheckReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			checkUpdates = append(checkUpdates, req)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 910})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/42/reviews":
			var req capturedReviewReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			reviewBodies = append(reviewBodies, req.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2002})
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
		PostInlineComments:     false,
		IncludeCoverageSummary: true,
	})

	// Construct huge FileDetails (~80 KB table) + many findings with ```suggestion blocks,
	// forcing BOTH Check Run and PR review into compact/bounded output.
	fileDetails := make([]domain.FileCoverageDetail, 0, 1200)
	unreviewed := make([]string, 0, 1200)
	for i := range 1200 {
		p := fmt.Sprintf("pkg/service/module_%04d/handler_implementation.go", i)
		fileDetails = append(fileDetails, domain.FileCoverageDetail{
			Path:              p,
			ChangeStatus:      "modified",
			ChangedHunks:      []domain.LineRange{{StartLine: 1, EndLine: 5}},
			InspectedByAgents: []string{"correctness"},
		})
		unreviewed = append(unreviewed, p)
	}
	findings := make([]output.Finding, 0, 120)
	for i := range 120 {
		findings = append(findings, output.Finding{
			FilePath:       "internal/auth/service.go",
			StartLine:      10,
			Severity:       output.SeverityHigh,
			Category:       "correctness",
			Title:          fmt.Sprintf("Finding %03d", i),
			Description:    strings.Repeat("Detailed rationale block. ", 25),
			SuggestionCode: "return fmt.Errorf(\"fixed\")",
			Confidence:     0.9,
			SourceAgents:   []string{"correctness"},
		})
	}

	task := testTaskContext()
	task.RunID = "run-compact-910"
	if err := configured.OnRunStarted(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	report := output.ReviewReport{
		Summary:  "```go\n" + strings.Repeat("// summary padding line\n", 10),
		Verdict:  output.VerdictComment,
		Findings: findings,
		Coverage: &domain.CoverageReport{
			RunID:                "run-compact-910",
			TotalChangedFiles:    1200,
			ReviewedChangedFiles: 0,
			HunkCoverageRatio:    0,
			UnreviewedFiles:      unreviewed,
			FileDetails:          fileDetails,
		},
	}
	// Close the fence cleanly for the summary in this test; a second check verifies unbalanced summary fences.
	report.Summary += "\n```"

	raw, _ := json.Marshal(report)
	if err := configured.Publish(context.Background(), task, raw); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var checkText, checkSummary, reviewBody string
	mu.Lock()
	if len(checkUpdates) != 1 || len(reviewBodies) != 1 {
		mu.Unlock()
		t.Fatalf("expected 1 check update and 1 review, got checks=%d reviews=%d", len(checkUpdates), len(reviewBodies))
	}
	checkText = checkUpdates[0].Output.Text
	checkSummary = checkUpdates[0].Output.Summary
	reviewBody = reviewBodies[0]
	mu.Unlock()

	// 1. Check Run must NEVER self-reference "see the Check Run" or "see Check Run"
	for _, s := range []string{checkText, checkSummary} {
		if strings.Contains(s, "Check Run") {
			t.Fatalf("Check Run output must not self-reference Check Run:\n%s", s)
		}
		if !strings.Contains(s, "GET /api/v1/runs/run-compact-910/coverage") {
			t.Fatalf("expected API coverage hint in compacted Check Run output:\n%s", s)
		}
	}
	// 2. Because Check Run itself is compacted, PR review must also point to the API endpoint, not Check Run.
	if strings.Contains(reviewBody, "see the Check Run") || strings.Contains(reviewBody, "see Check Run") {
		t.Fatalf("PR review must not point to compacted Check Run:\n%s", reviewBody)
	}
	if !strings.Contains(reviewBody, "GET /api/v1/runs/run-compact-910/coverage") {
		t.Fatalf("expected API coverage hint in PR review body:\n%s", reviewBody)
	}
	// 3. Findings were omitted cleanly on boundary and <details> / fences remain balanced.
	for name, md := range map[string]string{"reviewBody": reviewBody, "checkText": checkText, "checkSummary": checkSummary} {
		if len(md) > 65536 {
			t.Fatalf("%s exceeds GitHub limit (%d)", name, len(md))
		}
		if !utf8.ValidString(md) {
			t.Fatalf("%s has invalid UTF-8", name)
		}
		if strings.Count(md, "<details>") != strings.Count(md, "</details>") {
			t.Fatalf("%s has unbalanced <details> tags: open=%d close=%d",
				name, strings.Count(md, "<details>"), strings.Count(md, "</details>"))
		}
		fenceCount := 0
		for line := range strings.SplitSeq(md, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				fenceCount++
			}
		}
		if fenceCount%2 != 0 {
			t.Fatalf("%s has odd number of code fence markers (%d)", name, fenceCount)
		}
	}
	if !strings.Contains(reviewBody, "more finding(s) omitted to fit GitHub's size limit") {
		t.Fatalf("expected finding boundary omission notice in reviewBody")
	}

	// 4. Verify hard truncation inside an unclosed ```go code fence closes the fence cleanly.
	task2 := testTaskContext()
	task2.RunID = "run-fence-cut"
	fenceReport := output.ReviewReport{
		Summary: "```go\n" + strings.Repeat("var x = 1\n", 8000) + "```",
		Verdict: output.VerdictComment,
	}
	rawFence, _ := json.Marshal(fenceReport)
	if err := configured.Publish(context.Background(), task2, rawFence); err != nil {
		t.Fatalf("Publish fenceReport: %v", err)
	}
	mu.Lock()
	lastBody := reviewBodies[len(reviewBodies)-1]
	mu.Unlock()
	if len(lastBody) > 65536 {
		t.Fatalf("truncated fence body exceeds limit: %d", len(lastBody))
	}
	fenceCount := 0
	for line := range strings.SplitSeq(lastBody, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenceCount++
		}
	}
	if fenceCount == 0 || fenceCount%2 != 0 {
		t.Fatalf("expected balanced even number of code fences after hard truncation, got %d", fenceCount)
	}
}

func TestGitHubSink_EnforcesVerdictsAndHTTP422Fallback(t *testing.T) {
	cases := []struct {
		name           string
		verdict        output.Verdict
		rejectApprove  bool
		wantEvent      string
		wantConclusion string
	}{
		{
			name:           "approve",
			verdict:        output.VerdictApprove,
			wantEvent:      "APPROVE",
			wantConclusion: "success",
		},
		{
			name:           "request_changes",
			verdict:        output.VerdictRequestChanges,
			wantEvent:      "REQUEST_CHANGES",
			wantConclusion: "failure",
		},
		{
			name:           "comment",
			verdict:        output.VerdictComment,
			wantEvent:      "COMMENT",
			wantConclusion: "neutral",
		},
		{
			name:           "approve_422_fallback_to_comment",
			verdict:        output.VerdictApprove,
			rejectApprove:  true,
			wantEvent:      "COMMENT",
			wantConclusion: "success",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu          sync.Mutex
				lastEvent   string
				lastCheck   capturedCheckReq
				reviewCalls int
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs":
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 101})
				case r.Method == http.MethodPatch && r.URL.Path == "/repos/shohei-yamagiwa/mochi/check-runs/101":
					_ = json.NewDecoder(r.Body).Decode(&lastCheck)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 101})
				case r.Method == http.MethodPost && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/42/reviews":
					reviewCalls++
					var req capturedReviewReq
					_ = json.NewDecoder(r.Body).Decode(&req)
					if tc.rejectApprove && req.Event == "APPROVE" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_ = json.NewEncoder(w).Encode(map[string]any{"message": "Can not approve your own pull request"})
						return
					}
					lastEvent = req.Event
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 202})
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
				Type: "github_pr_review",
			})

			task := testTaskContext()
			task.RunID = "run-verdict-" + tc.name
			if err := configured.OnRunStarted(context.Background(), task); err != nil {
				t.Fatalf("OnRunStarted: %v", err)
			}
			raw, _ := json.Marshal(output.ReviewReport{
				Summary: "Verdict check",
				Verdict: tc.verdict,
			})
			if err := configured.Publish(context.Background(), task, raw); err != nil {
				t.Fatalf("Publish: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if lastEvent != tc.wantEvent {
				t.Fatalf("Event = %q, want %q", lastEvent, tc.wantEvent)
			}
			if lastCheck.Conclusion != tc.wantConclusion {
				t.Fatalf("Conclusion = %q, want %q", lastCheck.Conclusion, tc.wantConclusion)
			}
			if tc.rejectApprove && reviewCalls != 2 {
				t.Fatalf("reviewCalls = %d, want 2 after 422 fallback", reviewCalls)
			}
		})
	}
}
