package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/go-github/v93/github"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/input"
	ghinput "github.com/shohei-yamagiwa/mochi/internal/input/github"
	"github.com/shohei-yamagiwa/mochi/internal/output"
)

const (
	sinkName           = "github_pr_review"
	defaultCheckName   = "Mochi Code Review"
	maxCheckTextBytes  = 60000
	maxReviewBodyBytes = 60000 // Comfortably below GitHub's 65,536-character PR review body limit.
	maxInlineComments  = 50
)

// Option configures a GitHub PR Review OutputSink.
type Option func(*Sink)

// WithCheckName sets the GitHub Check Run name created during workflow execution.
func WithCheckName(name string) Option {
	return func(s *Sink) {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			s.checkName = trimmed
		}
	}
}

// WithDefaultConfig sets fallback SinkConfig values when WithConfig is not invoked.
func WithDefaultConfig(cfg domain.SinkConfig) Option {
	return func(s *Sink) {
		s.cfg = cfg
	}
}

type checkRunTracker struct {
	mu  sync.Mutex
	ids map[string]int64
}

func (t *checkRunTracker) set(runID string, checkRunID int64) {
	if t == nil || runID == "" || checkRunID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ids[runID] = checkRunID
}

func (t *checkRunTracker) get(runID string) (int64, bool) {
	if t == nil || runID == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.ids[runID]
	return id, ok
}

func (t *checkRunTracker) take(runID string) (int64, bool) {
	if t == nil || runID == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.ids[runID]
	if ok {
		delete(t.ids, runID)
	}
	return id, ok
}

func (t *checkRunTracker) delete(runID string) {
	if t == nil || runID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.ids, runID)
}

// Sink implements output.OutputSink (and output.ConfigurableSink) by publishing
// GitHub Check Runs, diff-mapped inline PR review comments, and collapsible coverage tables.
type Sink struct {
	clients   ghinput.ClientFactory
	checkName string
	cfg       domain.SinkConfig
	tracker   *checkRunTracker
}

var (
	_ output.OutputSink       = (*Sink)(nil)
	_ output.ConfigurableSink = (*Sink)(nil)
)

// NewSink creates a GitHubPRReviewSink backed by clients.
func NewSink(clients ghinput.ClientFactory, opts ...Option) (*Sink, error) {
	if clients == nil {
		return nil, fmt.Errorf("github output sink: client factory must not be nil")
	}
	s := &Sink{
		clients:   clients,
		checkName: defaultCheckName,
		cfg: domain.SinkConfig{
			Type:                   sinkName,
			PostInlineComments:     true,
			IncludeCoverageSummary: true,
			MinSeverity:            string(output.SeverityLow),
			MinConfidence:          0.7,
			VerdictMode:            "comment_only",
		},
		tracker: &checkRunTracker{
			ids: make(map[string]int64),
		},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

// Name returns "github_pr_review".
func (s *Sink) Name() string {
	return sinkName
}

// WithConfig returns a shallow clone configured with per-workflow SinkConfig while
// sharing the underlying Check Run ID tracker with the parent sink.
func (s *Sink) WithConfig(cfg domain.SinkConfig) output.OutputSink {
	cp := *s
	cp.cfg = cfg
	if name, ok := cfg.Options["check_name"].(string); ok && strings.TrimSpace(name) != "" {
		cp.checkName = strings.TrimSpace(name)
	}
	return &cp
}

// OnRunStarted creates an in-progress GitHub Check Run for the PR head commit.
func (s *Sink) OnRunStarted(ctx context.Context, task *domain.TaskContext) error {
	target := extractTarget(task)
	if target.owner == "" || target.repo == "" || target.headSHA == "" {
		return nil
	}

	client, err := s.clients.ClientForInstallation(ctx, target.installationID)
	if err != nil {
		return fmt.Errorf("github output client: %w", err)
	}

	summary := "Mochi multi-agent code review is running."
	if target.prNumber > 0 {
		summary = fmt.Sprintf("Mochi multi-agent code review is running for PR #%d (run `%s`).", target.prNumber, task.RunID)
	}

	cr, _, err := client.Checks.CreateCheckRun(ctx, target.owner, target.repo, github.CreateCheckRunOptions{
		Name:       s.checkName,
		HeadSHA:    target.headSHA,
		ExternalID: github.Ptr(task.RunID),
		Status:     github.Ptr("in_progress"),
		StartedAt:  &github.Timestamp{Time: time.Now().UTC()},
		Output: &github.CheckRunOutput{
			Title:   github.Ptr("Mochi code review in progress"),
			Summary: github.Ptr(summary),
		},
	})
	if err != nil {
		return fmt.Errorf("create github check run: %w", err)
	}
	if cr != nil && cr.GetID() > 0 {
		s.tracker.set(task.RunID, cr.GetID())
	}
	return nil
}

// Publish formats the ReviewReport, posts the PR Review with diff-mapped inline comments
// (falling back cleanly for out-of-diff lines or HTTP 422), and completes the Check Run.
func (s *Sink) Publish(ctx context.Context, task *domain.TaskContext, artifact json.RawMessage) error {
	if len(artifact) == 0 {
		return fmt.Errorf("github output sink: empty artifact")
	}
	var report output.ReviewReport
	if err := json.Unmarshal(artifact, &report); err != nil {
		return fmt.Errorf("github output sink: unmarshal review report: %w", err)
	}

	target := extractTarget(task)
	if target.owner == "" || target.repo == "" {
		return fmt.Errorf("github output sink: repository owner and name are required")
	}

	client, err := s.clients.ClientForInstallation(ctx, target.installationID)
	if err != nil {
		return fmt.Errorf("github output client: %w", err)
	}

	filtered, droppedCount := filterFindings(report.Findings, s.cfg.MinSeverity, s.cfg.MinConfidence)
	validDiffLines := collectValidDiffLines(task)

	var inlineFindings []output.Finding
	var draftComments []*github.DraftReviewComment
	var summaryFindings []output.Finding

	for _, f := range filtered {
		if s.cfg.PostInlineComments && target.prNumber > 0 && len(draftComments) < maxInlineComments {
			if comment, ok := buildInlineComment(f, validDiffLines); ok {
				inlineFindings = append(inlineFindings, f)
				draftComments = append(draftComments, comment)
				continue
			}
		}
		summaryFindings = append(summaryFindings, f)
	}

	body := renderCappedReviewBody(&report, summaryFindings, droppedCount, target.headSHA, s.cfg.IncludeCoverageSummary, false, maxReviewBodyBytes)
	event := s.resolveReviewEvent(report.Verdict)

	if target.prNumber > 0 {
		if err := s.createPRReviewWithFallbacks(ctx, client, target, event, body, draftComments, &report, inlineFindings, summaryFindings, droppedCount); err != nil {
			return err
		}
	}

	if checkRunID, ok := s.tracker.get(task.RunID); ok && checkRunID > 0 {
		conclusion := s.resolveCheckConclusion(report.Verdict)
		checkSummary := truncateUTF8(renderCheckSummary(&report, len(filtered), droppedCount, target.headSHA, s.cfg.IncludeCoverageSummary), maxCheckTextBytes)
		checkText := renderCappedReviewBody(&report, filtered, droppedCount, target.headSHA, s.cfg.IncludeCoverageSummary, true, maxCheckTextBytes)
		title := fmt.Sprintf("Mochi Review: %s (%d findings)", report.Verdict, len(filtered))

		_, _, err := client.Checks.UpdateCheckRun(ctx, target.owner, target.repo, checkRunID, github.UpdateCheckRunOptions{
			Name:        s.checkName,
			Status:      github.Ptr("completed"),
			Conclusion:  github.Ptr(conclusion),
			CompletedAt: &github.Timestamp{Time: time.Now().UTC()},
			Output: &github.CheckRunOutput{
				Title:   github.Ptr(title),
				Summary: github.Ptr(checkSummary),
				Text:    github.Ptr(checkText),
			},
		})
		if err != nil {
			return fmt.Errorf("update github check run %d: %w", checkRunID, err)
		}
		s.tracker.delete(task.RunID)
	}

	return nil
}

// OnRunFailed marks any active GitHub Check Run as completed with failure, cancelled, or timed_out.
func (s *Sink) OnRunFailed(ctx context.Context, task *domain.TaskContext, runErr error) error {
	if task == nil {
		return nil
	}
	checkRunID, ok := s.tracker.take(task.RunID)
	if !ok || checkRunID <= 0 {
		return nil
	}
	target := extractTarget(task)
	if target.owner == "" || target.repo == "" {
		return nil
	}

	client, err := s.clients.ClientForInstallation(ctx, target.installationID)
	if err != nil {
		return fmt.Errorf("github output client: %w", err)
	}

	conclusion := "failure"
	title := "Mochi code review failed"
	switch {
	case errors.Is(runErr, context.Canceled):
		conclusion = "cancelled"
		title = "Mochi code review cancelled"
	case errors.Is(runErr, context.DeadlineExceeded):
		conclusion = "timed_out"
		title = "Mochi code review timed out"
	}

	errText := "Workflow execution did not complete."
	if runErr != nil {
		errText = runErr.Error()
	}

	_, _, updateErr := client.Checks.UpdateCheckRun(ctx, target.owner, target.repo, checkRunID, github.UpdateCheckRunOptions{
		Name:        s.checkName,
		Status:      github.Ptr("completed"),
		Conclusion:  github.Ptr(conclusion),
		CompletedAt: &github.Timestamp{Time: time.Now().UTC()},
		Output: &github.CheckRunOutput{
			Title:   github.Ptr(title),
			Summary: github.Ptr(fmt.Sprintf("Run `%s` ended with status `%s`.\n\n```\n%s\n```", task.RunID, conclusion, truncateUTF8(errText, 4000))),
		},
	})
	if updateErr != nil {
		return fmt.Errorf("update failed check run %d: %w", checkRunID, updateErr)
	}
	return nil
}

func (s *Sink) createPRReviewWithFallbacks(
	ctx context.Context,
	client *github.Client,
	target prTarget,
	event string,
	body string,
	comments []*github.DraftReviewComment,
	report *output.ReviewReport,
	inlineFindings []output.Finding,
	summaryFindings []output.Finding,
	droppedCount int,
) error {
	req := &github.PullRequestReviewRequest{
		Body:     github.Ptr(body),
		Event:    github.Ptr(event),
		Comments: comments,
	}
	if target.headSHA != "" {
		req.CommitID = github.Ptr(target.headSHA)
	}

	_, resp, err := client.PullRequests.CreateReview(ctx, target.owner, target.repo, target.prNumber, req)
	if err == nil {
		return nil
	}

	if isUnprocessableEntity(resp, err) && len(comments) > 0 {
		// Inline diff comment placement failed on GitHub's side (e.g., stale commit or unsupported line).
		// Merge inline findings into the review summary body and retry without inline comments.
		combined := make([]output.Finding, 0, len(inlineFindings)+len(summaryFindings))
		combined = append(combined, inlineFindings...)
		combined = append(combined, summaryFindings...)
		fallbackBody := renderCappedReviewBody(report, combined, droppedCount, target.headSHA, s.cfg.IncludeCoverageSummary, true, maxReviewBodyBytes)
		fallbackReq := &github.PullRequestReviewRequest{
			Body:  github.Ptr(fallbackBody),
			Event: github.Ptr(event),
		}
		if target.headSHA != "" {
			fallbackReq.CommitID = github.Ptr(target.headSHA)
		}
		_, resp2, err2 := client.PullRequests.CreateReview(ctx, target.owner, target.repo, target.prNumber, fallbackReq)
		if err2 == nil {
			return nil
		}
		if isUnprocessableEntity(resp2, err2) && event != "COMMENT" {
			fallbackReq.Event = github.Ptr("COMMENT")
			if _, _, err3 := client.PullRequests.CreateReview(ctx, target.owner, target.repo, target.prNumber, fallbackReq); err3 == nil {
				return nil
			}
		}
		return fmt.Errorf("create github pr review (inline fallback): %w", err2)
	}

	if isUnprocessableEntity(resp, err) && event != "COMMENT" {
		req.Event = github.Ptr("COMMENT")
		if _, _, err2 := client.PullRequests.CreateReview(ctx, target.owner, target.repo, target.prNumber, req); err2 == nil {
			return nil
		}
	}

	return fmt.Errorf("create github pr review: %w", err)
}

func (s *Sink) resolveReviewEvent(verdict output.Verdict) string {
	if strings.EqualFold(strings.TrimSpace(s.cfg.VerdictMode), "enforce") {
		switch verdict {
		case output.VerdictApprove:
			return "APPROVE"
		case output.VerdictRequestChanges:
			return "REQUEST_CHANGES"
		}
	}
	return "COMMENT"
}

func (s *Sink) resolveCheckConclusion(verdict output.Verdict) string {
	if strings.EqualFold(strings.TrimSpace(s.cfg.VerdictMode), "enforce") {
		switch verdict {
		case output.VerdictApprove:
			return "success"
		case output.VerdictRequestChanges:
			return "failure"
		default:
			return "neutral"
		}
	}
	if verdict == output.VerdictApprove {
		return "success"
	}
	return "neutral"
}

func filterFindings(findings []output.Finding, minSev string, minConf float64) (kept []output.Finding, dropped int) {
	for _, f := range findings {
		if !output.SeverityMeetsMinimum(f.Severity, minSev) {
			dropped++
			continue
		}
		if minConf > 0 && f.Confidence < minConf {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	return kept, dropped
}

func collectValidDiffLines(task *domain.TaskContext) map[string]map[int]bool {
	out := make(map[string]map[int]bool)
	if task == nil || task.Changeset == nil {
		return out
	}
	if strings.TrimSpace(task.Changeset.UnifiedDiff) != "" {
		out = input.ParseDiffLinesByFile(task.Changeset.UnifiedDiff)
	}
	for _, cf := range task.Changeset.ChangedFiles {
		if cf.Path == "" || cf.Status == "deleted" || strings.TrimSpace(cf.Patch) == "" {
			continue
		}
		if _, exists := out[cf.Path]; !exists {
			out[cf.Path] = input.ValidDiffLines(cf.Patch)
		}
	}
	return out
}

func buildInlineComment(f output.Finding, validByFile map[string]map[int]bool) (*github.DraftReviewComment, bool) {
	path := strings.TrimSpace(f.FilePath)
	if path == "" || f.StartLine <= 0 {
		return nil, false
	}
	validLines, ok := validByFile[path]
	if !ok || len(validLines) == 0 {
		return nil, false
	}

	startLine := f.StartLine
	endLine := f.EndLine
	if endLine <= 0 || endLine < startLine {
		endLine = startLine
	}

	for line := startLine; line <= endLine; line++ {
		if !validLines[line] {
			return nil, false
		}
	}

	body := formatInlineFindingBody(f)
	c := &github.DraftReviewComment{
		Path: github.Ptr(path),
		Body: github.Ptr(body),
		Side: github.Ptr("RIGHT"),
		Line: github.Ptr(endLine),
	}
	if endLine > startLine {
		c.StartSide = github.Ptr("RIGHT")
		c.StartLine = github.Ptr(startLine)
	}
	return c, true
}

func formatInlineFindingBody(f output.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s] %s**", strings.ToUpper(string(f.Severity)), f.Title)
	var meta []string
	if f.Category != "" {
		meta = append(meta, f.Category)
	}
	if f.Confidence > 0 {
		meta = append(meta, fmt.Sprintf("confidence: %.2f", f.Confidence))
	}
	if len(f.SourceAgents) > 0 {
		meta = append(meta, "agents: "+strings.Join(f.SourceAgents, ", "))
	}
	if len(meta) > 0 {
		fmt.Fprintf(&b, " _(%s)_", strings.Join(meta, " | "))
	}
	if desc := strings.TrimSpace(f.Description); desc != "" {
		fmt.Fprintf(&b, "\n\n%s", desc)
	}
	if sug := strings.TrimRight(f.SuggestionCode, "\r\n"); strings.TrimSpace(sug) != "" {
		fmt.Fprintf(&b, "\n\n```suggestion\n%s\n```", sug)
	}
	return b.String()
}

func renderCappedReviewBody(
	report *output.ReviewReport,
	findings []output.Finding,
	droppedCount int,
	headSHA string,
	includeCoverage bool,
	noteFallback bool,
	maxBytes int,
) string {
	body := renderReviewBody(report, findings, droppedCount, headSHA, includeCoverage, noteFallback, false)
	if maxBytes <= 0 || len(body) <= maxBytes {
		return body
	}
	if includeCoverage && report.Coverage != nil {
		body = renderReviewBody(report, findings, droppedCount, headSHA, includeCoverage, noteFallback, true)
		if len(body) <= maxBytes {
			return body
		}
	}
	return truncateUTF8(body, maxBytes)
}

func renderReviewBody(
	report *output.ReviewReport,
	findings []output.Finding,
	droppedCount int,
	headSHA string,
	includeCoverage bool,
	noteFallback bool,
	compactCoverage bool,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Mochi Code Review — `%s`\n\n", report.Verdict)
	if summary := strings.TrimSpace(report.Summary); summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}

	if len(report.FailedAgents) > 0 {
		fmt.Fprintf(&b, "> **Partial Review Notice**: The following specialist agents failed to complete: `%s`.\n\n",
			strings.Join(report.FailedAgents, "`, `"))
	}

	if noteFallback && len(findings) > 0 {
		b.WriteString("_All findings are listed below (inline diff comments were not attached)._\n\n")
	}

	if len(findings) > 0 {
		b.WriteString("### Findings\n\n")
		for _, f := range findings {
			loc := f.FilePath
			if f.StartLine > 0 {
				if f.EndLine > f.StartLine {
					loc = fmt.Sprintf("%s:%d-%d", f.FilePath, f.StartLine, f.EndLine)
				} else {
					loc = fmt.Sprintf("%s:%d", f.FilePath, f.StartLine)
				}
			}
			if loc != "" {
				fmt.Fprintf(&b, "- **[%s]** `%s` — **%s**", strings.ToUpper(string(f.Severity)), loc, f.Title)
			} else {
				fmt.Fprintf(&b, "- **[%s]** **%s**", strings.ToUpper(string(f.Severity)), f.Title)
			}
			if f.Category != "" || f.Confidence > 0 {
				fmt.Fprintf(&b, " _(%s, conf %.2f)_", f.Category, f.Confidence)
			}
			b.WriteByte('\n')
			if desc := strings.TrimSpace(f.Description); desc != "" {
				fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(desc, "\n", "\n  "))
			}
			if sug := strings.TrimRight(f.SuggestionCode, "\r\n"); strings.TrimSpace(sug) != "" {
				fmt.Fprintf(&b, "  ```suggestion\n  %s\n  ```\n", strings.ReplaceAll(sug, "\n", "\n  "))
			}
		}
		b.WriteByte('\n')
	}

	if droppedCount > 0 {
		fmt.Fprintf(&b, "_%d additional finding(s) omitted below configured severity/confidence thresholds._\n\n", droppedCount)
	}

	if report.TestExecution != nil {
		status := "did not run"
		if report.TestExecution.Ran {
			if report.TestExecution.Passed {
				status = "passed"
			} else {
				status = "failed"
			}
		}
		fmt.Fprintf(&b, "### Test Execution (%s)\n\n", status)
		if report.TestExecution.Summary != "" {
			b.WriteString(report.TestExecution.Summary)
			b.WriteString("\n\n")
		}
		if len(report.TestExecution.FailedTests) > 0 {
			fmt.Fprintf(&b, "Failed tests: `%s`\n\n", strings.Join(report.TestExecution.FailedTests, "`, `"))
		}
	}

	if includeCoverage && report.Coverage != nil {
		b.WriteString(renderCoverageDetails(report.Coverage, headSHA, compactCoverage))
	}

	return strings.TrimSpace(b.String())
}

func renderCheckSummary(report *output.ReviewReport, keptCount, droppedCount int, headSHA string, includeCoverage bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Verdict:** `%s` | **Findings:** %d", report.Verdict, keptCount)
	if droppedCount > 0 {
		fmt.Fprintf(&b, " (%d filtered)", droppedCount)
	}
	b.WriteString("\n\n")
	if report.Summary != "" {
		b.WriteString(report.Summary)
		b.WriteString("\n\n")
	}
	if len(report.FailedAgents) > 0 {
		fmt.Fprintf(&b, "**Failed agents:** `%s`\n\n", strings.Join(report.FailedAgents, "`, `"))
	}
	if includeCoverage && report.Coverage != nil {
		details := renderCoverageDetails(report.Coverage, headSHA, false)
		if b.Len()+len(details) > maxCheckTextBytes {
			details = renderCoverageDetails(report.Coverage, headSHA, true)
		}
		b.WriteString(details)
	}
	return strings.TrimSpace(b.String())
}

func renderCoverageDetails(cov *domain.CoverageReport, headSHA string, compact bool) string {
	sha := cov.HeadSHA
	if sha == "" {
		sha = headSHA
	}
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if sha == "" {
		sha = "HEAD"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<details>\n<summary>Review Coverage (%d/%d files, %.0f%% hunks inspected at commit %s)</summary>\n\n",
		cov.ReviewedChangedFiles,
		cov.TotalChangedFiles,
		cov.HunkCoverageRatio*100,
		sha,
	)

	if len(cov.UnreviewedFiles) > 0 {
		const maxUnreviewedListed = 50
		if compact && len(cov.UnreviewedFiles) > maxUnreviewedListed {
			fmt.Fprintf(&b, "**Unreviewed changed files (%d total, showing first %d):** `%s`, ... (see Check Run for full list)\n\n",
				len(cov.UnreviewedFiles), maxUnreviewedListed, strings.Join(cov.UnreviewedFiles[:maxUnreviewedListed], "`, `"))
		} else {
			fmt.Fprintf(&b, "**Unreviewed changed files:** `%s`\n\n", strings.Join(cov.UnreviewedFiles, "`, `"))
		}
	}

	if compact {
		if len(cov.FileDetails) > 0 {
			fmt.Fprintf(&b, "_Per-file coverage table (%d files) omitted to fit GitHub's size limit; see Check Run for full details._\n\n", len(cov.FileDetails))
		}
		b.WriteString("</details>\n")
		return b.String()
	}

	if len(cov.FileDetails) > 0 {
		b.WriteString("| File | Status | Inspected By | Hunks | Uncovered | Findings |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		for _, fd := range cov.FileDetails {
			agents := "-"
			if len(fd.InspectedByAgents) > 0 {
				agents = strings.Join(fd.InspectedByAgents, ", ")
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %d | %d | %d |\n",
				fd.Path,
				fd.ChangeStatus,
				agents,
				len(fd.ChangedHunks),
				len(fd.UncoveredHunks),
				fd.FindingsCount,
			)
		}
		b.WriteByte('\n')
	}

	if len(cov.ContextFilesRead) > 0 {
		fmt.Fprintf(&b, "Context files read: `%s`\n\n", strings.Join(cov.ContextFilesRead, "`, `"))
	}

	b.WriteString("</details>\n")
	return b.String()
}

type prTarget struct {
	owner          string
	repo           string
	prNumber       int
	headSHA        string
	installationID int64
}

func extractTarget(task *domain.TaskContext) prTarget {
	var t prTarget
	if task == nil {
		return t
	}
	if task.Changeset != nil {
		t.owner = task.Changeset.RepoOwner
		t.repo = task.Changeset.RepoName
		t.prNumber = task.Changeset.PRNumber
		t.headSHA = task.Changeset.HeadSHA
		t.installationID = task.Changeset.InstallationID
	}
	if t.headSHA == "" {
		t.headSHA = task.Workspace.Ref
	}
	if task.SinkMetadata != nil {
		if t.owner == "" {
			if v, ok := task.SinkMetadata["owner"].(string); ok {
				t.owner = v
			}
		}
		if t.repo == "" {
			if v, ok := task.SinkMetadata["repo"].(string); ok {
				t.repo = v
			}
		}
		if t.prNumber == 0 {
			if n, ok := intFromAny(task.SinkMetadata["pr_number"]); ok {
				t.prNumber = n
			}
		}
		if t.headSHA == "" {
			if v, ok := task.SinkMetadata["head_sha"].(string); ok {
				t.headSHA = v
			}
		}
		if t.installationID == 0 {
			if n, ok := int64FromAny(task.SinkMetadata["installation_id"]); ok {
				t.installationID = n
			}
		}
	}
	return t
}

func intFromAny(v any) (int, bool) {
	n, ok := int64FromAny(v)
	return int(n), ok
}

func int64FromAny(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func isUnprocessableEntity(resp *github.Response, err error) bool {
	if resp != nil && resp.StatusCode == http.StatusUnprocessableEntity {
		return true
	}
	var ghErr *github.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
		return true
	}
	return false
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	const suffix = "\n... [truncated]"
	limit := maxBytes - len(suffix)
	if limit <= 0 {
		limit = maxBytes
		for limit > 0 && !utf8.RuneStart(s[limit]) {
			limit--
		}
		return s[:limit]
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + suffix
}
