package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/google/go-github/v93/github"
	"github.com/shohei-yamagiwa/mochi/internal/config"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/input"
	"github.com/shohei-yamagiwa/mochi/internal/orchestrator"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
)

const (
	adapterName           = "github_pr"
	defaultCommentCommand = "/mochi review"
)

var (
	// ErrIgnoredEvent is returned when a valid GitHub webhook does not match configured trigger filters.
	ErrIgnoredEvent = errors.New("github webhook event ignored")
	// ErrInvalidSignature is returned when X-Hub-Signature-256 fails constant-time HMAC verification.
	ErrInvalidSignature = errors.New("invalid github webhook signature")

	defaultPREvents            = []string{"opened", "synchronize", "reopened", "ready_for_review"}
	defaultCommentAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}
)

// WebhookEvent represents a raw HTTP webhook delivery received from GitHub.
type WebhookEvent struct {
	EventType    string
	DeliveryID   string
	Signature256 string
	Payload      []byte
	Triggers     *domain.TriggerConfig
	WorkflowName string
}

// PRTarget identifies a GitHub Pull Request to fetch and normalize into a TaskContext.
type PRTarget struct {
	Owner          string
	Repo           string
	PRNumber       int
	InstallationID int64
	TriggerEvent   string
	WorkflowName   string
	Triggers       *domain.TriggerConfig
}

// WebhookCandidate represents a verified GitHub PR or PR-comment webhook prior to
// evaluating repository-defined workflows under .mochi/workflows/*.yaml.
type WebhookCandidate struct {
	Owner                    string
	Repo                     string
	PRNumber                 int
	InstallationID           int64
	TriggerEvent             string
	PRAction                 string
	CommentBody              string
	CommentAuthorAssociation string
}

// RepoWorkflowTask pairs a matched repository-owned workflow with its built TaskContext.
type RepoWorkflowTask struct {
	Workflow *domain.Workflow
	Task     *domain.TaskContext
}

// Option configures a GitHub PR InputAdapter.
type Option func(*Adapter)

// WithWebhookSecret configures the HMAC-SHA256 secret used to verify X-Hub-Signature-256.
func WithWebhookSecret(secret string) Option {
	return func(a *Adapter) {
		a.webhookSecret = secret
	}
}

// WithAllowInsecureNoWebhookSecret permits skipping webhook signature verification when no secret is configured.
// Only enable this explicitly for local development/testing.
func WithAllowInsecureNoWebhookSecret(allow bool) Option {
	return func(a *Adapter) {
		a.allowInsecureNoSecret = allow
	}
}

// WithDefaultTriggers configures default PR event and comment-command trigger rules.
func WithDefaultTriggers(triggers domain.TriggerConfig) Option {
	return func(a *Adapter) {
		a.defaultTriggers = triggers
	}
}

// WithWorkflowName sets the default WorkflowName assigned to built TaskContexts.
func WithWorkflowName(name string) Option {
	return func(a *Adapter) {
		a.workflowName = strings.TrimSpace(name)
	}
}

// Adapter implements domain.InputAdapter for GitHub App webhooks and Pull Request events.
type Adapter struct {
	clients               ClientFactory
	webhookSecret         string
	allowInsecureNoSecret bool
	defaultTriggers       domain.TriggerConfig
	workflowName          string
}

var _ domain.InputAdapter = (*Adapter)(nil)

// NewAdapter creates a GitHub PR InputAdapter backed by clients.
func NewAdapter(clients ClientFactory, opts ...Option) (*Adapter, error) {
	if clients == nil {
		return nil, fmt.Errorf("github input adapter: client factory must not be nil")
	}
	a := &Adapter{
		clients: clients,
		defaultTriggers: domain.TriggerConfig{
			Events:                     slices.Clone(defaultPREvents),
			CommentCommand:             defaultCommentCommand,
			AllowedCommentAssociations: slices.Clone(defaultCommentAssociations),
			IgnoreDrafts:               true,
		},
		workflowName: "pr-review",
	}
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a, nil
}

// Name returns "github_pr".
func (a *Adapter) Name() string {
	return adapterName
}

// VerifySignature validates GitHub's X-Hub-Signature-256 header against payload using constant-time comparison.
// When no webhook secret is configured, verification fails closed with ErrInvalidSignature unless
// WithAllowInsecureNoWebhookSecret(true) is explicitly set.
func (a *Adapter) VerifySignature(payload []byte, signature256 string) error {
	if a.webhookSecret == "" {
		if a.allowInsecureNoSecret {
			return nil
		}
		return fmt.Errorf("%w: webhook secret is not configured", ErrInvalidSignature)
	}
	sigHeader := strings.TrimSpace(signature256)
	if !strings.HasPrefix(sigHeader, "sha256=") {
		return fmt.Errorf("%w: missing sha256= prefix", ErrInvalidSignature)
	}
	rawHex := strings.TrimPrefix(sigHeader, "sha256=")
	gotMAC, err := hex.DecodeString(rawHex)
	if err != nil {
		return fmt.Errorf("%w: malformed hex digest", ErrInvalidSignature)
	}
	mac := hmac.New(sha256.New, []byte(a.webhookSecret))
	_, _ = mac.Write(payload)
	expectedMAC := mac.Sum(nil)
	if !hmac.Equal(gotMAC, expectedMAC) {
		return ErrInvalidSignature
	}
	return nil
}

// ParseWebhookEvent validates and normalizes a raw GitHub webhook payload into a PRTarget
// or returns ErrIgnoredEvent when the event should not start a workflow run.
func (a *Adapter) ParseWebhookEvent(eventType string, payload []byte, overrideTriggers *domain.TriggerConfig) (*PRTarget, error) {
	triggers := a.effectiveTriggers(overrideTriggers)

	parsed, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		return nil, fmt.Errorf("parse github %q webhook: %w", eventType, err)
	}

	switch ev := parsed.(type) {
	case *github.PullRequestEvent:
		action := ev.GetAction()
		allowedEvents := triggers.Events
		if overrideTriggers == nil || overrideTriggers.Events == nil {
			if len(allowedEvents) == 0 {
				allowedEvents = defaultPREvents
			}
		}
		if !slices.Contains(allowedEvents, action) {
			return nil, fmt.Errorf("%w: pull_request action %q not in %v", ErrIgnoredEvent, action, allowedEvents)
		}
		pr := ev.GetPullRequest()
		if pr == nil {
			return nil, fmt.Errorf("pull_request webhook missing pull_request payload")
		}
		if triggers.IgnoreDrafts && pr.GetDraft() {
			return nil, fmt.Errorf("%w: pull_request #%d is a draft", ErrIgnoredEvent, pr.GetNumber())
		}
		if state := strings.TrimSpace(pr.GetState()); state != "" && !strings.EqualFold(state, "open") && action != "closed" {
			return nil, fmt.Errorf("%w: pull_request #%d state is %q", ErrIgnoredEvent, pr.GetNumber(), state)
		}
		if baseBranch := strings.TrimSpace(pr.GetBase().GetRef()); baseBranch != "" && !MatchBranches(triggers.BaseBranches, baseBranch) {
			return nil, fmt.Errorf("%w: pull_request #%d base branch %q does not match %v", ErrIgnoredEvent, pr.GetNumber(), baseBranch, triggers.BaseBranches)
		}
		if headBranch := strings.TrimSpace(pr.GetHead().GetRef()); headBranch != "" && !MatchBranches(triggers.HeadBranches, headBranch) {
			return nil, fmt.Errorf("%w: pull_request #%d head branch %q does not match %v", ErrIgnoredEvent, pr.GetNumber(), headBranch, triggers.HeadBranches)
		}
		owner, repo := extractRepoOwnerName(ev.GetRepo())
		if owner == "" || repo == "" {
			return nil, fmt.Errorf("pull_request webhook missing repository owner/name")
		}
		return &PRTarget{
			Owner:          owner,
			Repo:           repo,
			PRNumber:       ev.GetNumber(),
			InstallationID: ev.GetInstallation().GetID(),
			TriggerEvent:   "pull_request." + action,
			WorkflowName:   a.workflowName,
		}, nil

	case *github.IssueCommentEvent:
		if ev.GetAction() != "created" {
			return nil, fmt.Errorf("%w: issue_comment action %q is not created", ErrIgnoredEvent, ev.GetAction())
		}
		if ev.GetSender().GetType() == "Bot" {
			return nil, fmt.Errorf("%w: ignoring comment from bot sender", ErrIgnoredEvent)
		}
		issue := ev.GetIssue()
		if issue == nil || !issue.IsPullRequest() {
			return nil, fmt.Errorf("%w: issue_comment is not on a pull request", ErrIgnoredEvent)
		}
		if state := strings.TrimSpace(issue.GetState()); state != "" && !strings.EqualFold(state, "open") {
			return nil, fmt.Errorf("%w: issue_comment on non-open pull request #%d (state=%q)", ErrIgnoredEvent, issue.GetNumber(), state)
		}
		cmd := strings.TrimSpace(triggers.CommentCommand)
		if cmd == "" {
			return nil, fmt.Errorf("%w: comment_command trigger is disabled", ErrIgnoredEvent)
		}
		if !matchesCommentCommand(ev.GetComment().GetBody(), cmd) {
			return nil, fmt.Errorf("%w: comment does not invoke %q", ErrIgnoredEvent, cmd)
		}
		assoc := strings.TrimSpace(ev.GetComment().GetAuthorAssociation())
		if !isAllowedCommentAssociation(assoc, triggers.AllowedCommentAssociations) {
			return nil, fmt.Errorf("%w: comment author_association %q is not permitted", ErrIgnoredEvent, assoc)
		}
		owner, repo := extractRepoOwnerName(ev.GetRepo())
		if owner == "" || repo == "" {
			return nil, fmt.Errorf("issue_comment webhook missing repository owner/name")
		}
		return &PRTarget{
			Owner:          owner,
			Repo:           repo,
			PRNumber:       issue.GetNumber(),
			InstallationID: ev.GetInstallation().GetID(),
			TriggerEvent:   "issue_comment.command",
			WorkflowName:   a.workflowName,
		}, nil

	default:
		return nil, fmt.Errorf("%w: unsupported webhook event type %q", ErrIgnoredEvent, eventType)
	}
}

// ParseWebhookCandidate validates structural PR or PR-comment webhooks without filtering
// by workflow-specific trigger rules (events, comment_command, drafts, or branches),
// so each repository workflow under .mochi/workflows/*.yaml can evaluate its own triggers.
func (a *Adapter) ParseWebhookCandidate(eventType string, payload []byte) (*WebhookCandidate, error) {
	parsed, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		return nil, fmt.Errorf("parse github %q webhook: %w", eventType, err)
	}

	switch ev := parsed.(type) {
	case *github.PullRequestEvent:
		action := ev.GetAction()
		pr := ev.GetPullRequest()
		if pr == nil {
			return nil, fmt.Errorf("pull_request webhook missing pull_request payload")
		}
		if state := strings.TrimSpace(pr.GetState()); state != "" && !strings.EqualFold(state, "open") && action != "closed" {
			return nil, fmt.Errorf("%w: pull_request #%d state is %q", ErrIgnoredEvent, pr.GetNumber(), state)
		}
		owner, repo := extractRepoOwnerName(ev.GetRepo())
		if owner == "" || repo == "" {
			return nil, fmt.Errorf("pull_request webhook missing repository owner/name")
		}
		return &WebhookCandidate{
			Owner:          owner,
			Repo:           repo,
			PRNumber:       ev.GetNumber(),
			InstallationID: ev.GetInstallation().GetID(),
			TriggerEvent:   "pull_request." + action,
			PRAction:       action,
		}, nil

	case *github.IssueCommentEvent:
		if ev.GetAction() != "created" {
			return nil, fmt.Errorf("%w: issue_comment action %q is not created", ErrIgnoredEvent, ev.GetAction())
		}
		if ev.GetSender().GetType() == "Bot" {
			return nil, fmt.Errorf("%w: ignoring comment from bot sender", ErrIgnoredEvent)
		}
		issue := ev.GetIssue()
		if issue == nil || !issue.IsPullRequest() {
			return nil, fmt.Errorf("%w: issue_comment is not on a pull request", ErrIgnoredEvent)
		}
		if state := strings.TrimSpace(issue.GetState()); state != "" && !strings.EqualFold(state, "open") {
			return nil, fmt.Errorf("%w: issue_comment on non-open pull request #%d (state=%q)", ErrIgnoredEvent, issue.GetNumber(), state)
		}
		owner, repo := extractRepoOwnerName(ev.GetRepo())
		if owner == "" || repo == "" {
			return nil, fmt.Errorf("issue_comment webhook missing repository owner/name")
		}
		return &WebhookCandidate{
			Owner:                    owner,
			Repo:                     repo,
			PRNumber:                 issue.GetNumber(),
			InstallationID:           ev.GetInstallation().GetID(),
			TriggerEvent:             "issue_comment.command",
			CommentBody:              ev.GetComment().GetBody(),
			CommentAuthorAssociation: strings.TrimSpace(ev.GetComment().GetAuthorAssociation()),
		}, nil

	default:
		return nil, fmt.Errorf("%w: unsupported webhook event type %q", ErrIgnoredEvent, eventType)
	}
}

// BuildTaskContext normalizes a WebhookEvent or PRTarget into a canonical domain.TaskContext
// by fetching the PR metadata, unified diff, changed files, and a scoped read-only clone token.
func (a *Adapter) BuildTaskContext(ctx context.Context, rawEvent any) (*domain.TaskContext, error) {
	var target *PRTarget

	switch ev := rawEvent.(type) {
	case *WebhookEvent:
		if ev == nil {
			return nil, fmt.Errorf("github input adapter: webhook event must not be nil")
		}
		if err := a.VerifySignature(ev.Payload, ev.Signature256); err != nil {
			return nil, err
		}
		parsed, err := a.ParseWebhookEvent(ev.EventType, ev.Payload, ev.Triggers)
		if err != nil {
			return nil, err
		}
		if ev.WorkflowName != "" {
			parsed.WorkflowName = ev.WorkflowName
		}
		parsed.Triggers = ev.Triggers
		target = parsed
	case WebhookEvent:
		return a.BuildTaskContext(ctx, &ev)
	case *PRTarget:
		if ev == nil {
			return nil, fmt.Errorf("github input adapter: pr target must not be nil")
		}
		cp := *ev
		target = &cp
	case PRTarget:
		cp := ev
		target = &cp
	default:
		return nil, fmt.Errorf("github input adapter: unsupported raw event type %T", rawEvent)
	}

	if target.Owner == "" || target.Repo == "" || target.PRNumber <= 0 {
		return nil, fmt.Errorf("github input adapter: owner, repo, and positive pr_number are required")
	}

	client, err := a.clients.ClientForInstallation(ctx, target.InstallationID)
	if err != nil {
		return nil, fmt.Errorf("get github client: %w", err)
	}

	pr, _, err := client.PullRequests.Get(ctx, target.Owner, target.Repo, target.PRNumber)
	if err != nil {
		return nil, fmt.Errorf("get pull request %s/%s#%d: %w", target.Owner, target.Repo, target.PRNumber, err)
	}

	if shouldApplyWebhookFilters(target) {
		triggers := a.effectiveTriggers(target.Triggers)
		if target.TriggerEvent != "pull_request.closed" {
			if (pr.GetState() != "" && !strings.EqualFold(pr.GetState(), "open")) || pr.GetMerged() {
				return nil, fmt.Errorf("%w: pull_request #%d is closed/merged (state=%q merged=%v)", ErrIgnoredEvent, target.PRNumber, pr.GetState(), pr.GetMerged())
			}
		}
		if triggers.IgnoreDrafts && pr.GetDraft() {
			return nil, fmt.Errorf("%w: pull_request #%d is a draft", ErrIgnoredEvent, target.PRNumber)
		}
		if !MatchBranches(triggers.BaseBranches, pr.GetBase().GetRef()) {
			return nil, fmt.Errorf("%w: pull_request #%d base branch %q does not match %v", ErrIgnoredEvent, target.PRNumber, pr.GetBase().GetRef(), triggers.BaseBranches)
		}
		if !MatchBranches(triggers.HeadBranches, pr.GetHead().GetRef()) {
			return nil, fmt.Errorf("%w: pull_request #%d head branch %q does not match %v", ErrIgnoredEvent, target.PRNumber, pr.GetHead().GetRef(), triggers.HeadBranches)
		}
	}

	unifiedDiff, changedFiles, cloneToken, err := a.fetchPRDiffFilesAndToken(ctx, client, target.Owner, target.Repo, target.PRNumber, target.InstallationID)
	if err != nil {
		return nil, err
	}

	wfName := strings.TrimSpace(target.WorkflowName)
	if wfName == "" {
		wfName = a.workflowName
	}

	task := buildTaskContextFromPR(target.Owner, target.Repo, target.PRNumber, target.InstallationID, target.TriggerEvent, wfName, pr, unifiedDiff, changedFiles, cloneToken)
	task.DedupKey = fmt.Sprintf("github:%s/%s:pr:%d", target.Owner, target.Repo, target.PRNumber)
	return task, nil
}

// BuildTaskContextsForRepoWorkflows fetches the PR metadata, loads .mochi/workflows/*.yaml from the
// authoritative base branch/SHA (never the untrusted PR head branch), evaluates each workflow's triggers
// (events, comment_command, drafts, base_branches, head_branches), and builds one RepoWorkflowTask
// per matching workflow.
func (a *Adapter) BuildTaskContextsForRepoWorkflows(ctx context.Context, cand *WebhookCandidate) ([]*RepoWorkflowTask, error) {
	if cand == nil {
		return nil, fmt.Errorf("github input adapter: webhook candidate must not be nil")
	}
	if cand.Owner == "" || cand.Repo == "" || cand.PRNumber <= 0 {
		return nil, fmt.Errorf("github input adapter: owner, repo, and positive pr_number are required")
	}

	client, err := a.clients.ClientForInstallation(ctx, cand.InstallationID)
	if err != nil {
		return nil, fmt.Errorf("get github client: %w", err)
	}

	pr, _, err := client.PullRequests.Get(ctx, cand.Owner, cand.Repo, cand.PRNumber)
	if err != nil {
		return nil, fmt.Errorf("get pull request %s/%s#%d: %w", cand.Owner, cand.Repo, cand.PRNumber, err)
	}

	baseRef := strings.TrimSpace(pr.GetBase().GetSHA())
	if baseRef == "" {
		baseRef = strings.TrimSpace(pr.GetBase().GetRef())
	}
	if baseRef == "" {
		return nil, fmt.Errorf("pull request %s/%s#%d missing base ref", cand.Owner, cand.Repo, cand.PRNumber)
	}

	entries, err := listRepoDirAtRef(ctx, client, cand.Owner, cand.Repo, ".mochi/workflows", baseRef)
	if err != nil {
		return nil, fmt.Errorf("list .mochi/workflows from base ref %q: %w", baseRef, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: no .mochi/workflows/*.yaml files found on base ref %q", ErrIgnoredEvent, baseRef)
	}

	var matched []*domain.Workflow
	seenNames := make(map[string]string)
	for _, entry := range entries {
		yamlBytes, found, err := fetchRepoFileAtRef(ctx, client, cand.Owner, cand.Repo, entry.GetPath(), baseRef)
		if err != nil {
			return nil, fmt.Errorf("fetch %q from base ref %q: %w", entry.GetPath(), baseRef, err)
		}
		if !found {
			continue
		}
		wf, err := config.ParseRepoWorkflow(yamlBytes)
		if err != nil {
			return nil, fmt.Errorf("parse repo workflow %q on base ref %q: %w", entry.GetPath(), baseRef, err)
		}
		if prevPath, dup := seenNames[wf.Name]; dup {
			return nil, fmt.Errorf("duplicate workflow name %q in %q and %q", wf.Name, prevPath, entry.GetPath())
		}
		seenNames[wf.Name] = entry.GetPath()

		if a.MatchesTrigger(wf.Input.Triggers, cand, pr) {
			matched = append(matched, wf)
		}
	}

	if len(matched) == 0 {
		return nil, fmt.Errorf("%w: no workflows in .mochi/workflows matched %s (base=%q head=%q)", ErrIgnoredEvent, cand.TriggerEvent, pr.GetBase().GetRef(), pr.GetHead().GetRef())
	}

	unifiedDiff, changedFiles, cloneToken, err := a.fetchPRDiffFilesAndToken(ctx, client, cand.Owner, cand.Repo, cand.PRNumber, cand.InstallationID)
	if err != nil {
		return nil, err
	}

	results := make([]*RepoWorkflowTask, 0, len(matched))
	cleanupAll := func() {
		for _, r := range results {
			if r != nil && r.Task != nil && r.Task.PromptDir != "" {
				_ = os.RemoveAll(r.Task.PromptDir)
			}
		}
	}

	for _, wf := range matched {
		promptDir, err := materializeWorkflowRepoPrompts(ctx, client, cand.Owner, cand.Repo, baseRef, wf)
		if err != nil {
			cleanupAll()
			return nil, err
		}
		task := buildTaskContextFromPR(cand.Owner, cand.Repo, cand.PRNumber, cand.InstallationID, cand.TriggerEvent, wf.Name, pr, unifiedDiff, changedFiles, cloneToken)
		task.DedupKey = fmt.Sprintf("github:%s/%s:pr:%d:workflow:%s", cand.Owner, cand.Repo, cand.PRNumber, wf.Name)
		task.PromptDir = promptDir
		results = append(results, &RepoWorkflowTask{
			Workflow: wf,
			Task:     task,
		})
	}
	return results, nil
}

// MatchesTrigger evaluates a workflow's TriggerConfig against a WebhookCandidate and authoritative PR metadata.
func (a *Adapter) MatchesTrigger(cfg domain.TriggerConfig, cand *WebhookCandidate, pr *github.PullRequest) bool {
	if cand == nil || pr == nil {
		return false
	}
	triggers := a.effectiveTriggers(&cfg)

	if cand.PRAction != "closed" {
		if (pr.GetState() != "" && !strings.EqualFold(pr.GetState(), "open")) || pr.GetMerged() {
			return false
		}
	}
	if triggers.IgnoreDrafts && pr.GetDraft() {
		return false
	}
	if !MatchBranches(triggers.BaseBranches, pr.GetBase().GetRef()) {
		return false
	}
	if !MatchBranches(triggers.HeadBranches, pr.GetHead().GetRef()) {
		return false
	}

	if strings.HasPrefix(cand.TriggerEvent, "pull_request.") {
		allowedEvents := triggers.Events
		if cfg.Events == nil && len(allowedEvents) == 0 {
			allowedEvents = defaultPREvents
		}
		return slices.Contains(allowedEvents, cand.PRAction)
	}
	if cand.TriggerEvent == "issue_comment.command" {
		cmd := strings.TrimSpace(triggers.CommentCommand)
		if cmd == "" {
			return false
		}
		if !matchesCommentCommand(cand.CommentBody, cmd) {
			return false
		}
		return isAllowedCommentAssociation(cand.CommentAuthorAssociation, triggers.AllowedCommentAssociations)
	}
	return false
}

func (a *Adapter) fetchPRDiffFilesAndToken(
	ctx context.Context,
	client *github.Client,
	owner, repo string,
	prNumber int,
	installationID int64,
) (string, []domain.ChangedFile, string, error) {
	unifiedDiff, _, err := client.PullRequests.GetRaw(ctx, owner, repo, prNumber, github.RawOptions{
		Type: github.Diff,
	})
	if err != nil {
		return "", nil, "", fmt.Errorf("get pull request raw diff %s/%s#%d: %w", owner, repo, prNumber, err)
	}

	commitFiles, err := listAllPRFiles(ctx, client, owner, repo, prNumber)
	if err != nil {
		return "", nil, "", fmt.Errorf("list pull request files %s/%s#%d: %w", owner, repo, prNumber, err)
	}

	parsedUnified := input.ParseUnifiedDiff(unifiedDiff)
	parsedByPath := make(map[string]domain.ChangedFile, len(parsedUnified))
	for _, pf := range parsedUnified {
		parsedByPath[pf.Path] = pf
	}

	changedFiles := make([]domain.ChangedFile, 0, len(commitFiles))
	for _, cf := range commitFiles {
		if cf == nil || strings.TrimSpace(cf.GetFilename()) == "" {
			continue
		}
		path := cf.GetFilename()
		status := normalizeGitHubFileStatus(cf.GetStatus())
		patch := cf.GetPatch()
		var hunks []domain.LineRange
		if status != "deleted" {
			if strings.TrimSpace(patch) != "" {
				hunks = input.ParsePatchHunks(patch)
			} else if fallback, ok := parsedByPath[path]; ok {
				patch = fallback.Patch
				hunks = fallback.ChangedHunks
			}
		}
		changedFiles = append(changedFiles, domain.ChangedFile{
			Path:         path,
			PreviousPath: cf.GetPreviousFilename(),
			Status:       status,
			Additions:    cf.GetAdditions(),
			Deletions:    cf.GetDeletions(),
			Patch:        patch,
			ChangedHunks: hunks,
		})
	}
	if len(changedFiles) == 0 && len(parsedUnified) > 0 {
		changedFiles = parsedUnified
	}

	cloneToken, err := a.clients.CloneTokenForRepo(ctx, installationID, repo)
	if err != nil {
		return "", nil, "", fmt.Errorf("acquire clone token: %w", err)
	}
	return unifiedDiff, changedFiles, cloneToken, nil
}

func buildTaskContextFromPR(
	owner, repo string,
	prNumber int,
	installationID int64,
	triggerEvent string,
	wfName string,
	pr *github.PullRequest,
	unifiedDiff string,
	changedFiles []domain.ChangedFile,
	cloneToken string,
) *domain.TaskContext {
	baseBranch := pr.GetBase().GetRef()
	baseSHA := pr.GetBase().GetSHA()
	headBranch := pr.GetHead().GetRef()
	headSHA := pr.GetHead().GetSHA()
	repoURL := ""
	if baseRepo := pr.GetBase().GetRepo(); baseRepo != nil {
		repoURL = baseRepo.GetCloneURL()
	}
	if repoURL == "" {
		repoURL = fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	}

	isFork := false
	if headRepo := pr.GetHead().GetRepo(); headRepo != nil {
		isFork = headRepo.GetFullName() != fmt.Sprintf("%s/%s", owner, repo)
	}

	var fetchRefs []string
	if prNumber > 0 {
		fetchRefs = append(fetchRefs, fmt.Sprintf("refs/pull/%d/head", prNumber))
	}

	filesCopy := slices.Clone(changedFiles)

	return &domain.TaskContext{
		WorkflowName: wfName,
		TriggerType:  adapterName,
		Workspace: domain.WorkspaceSpec{
			RepoURL:    repoURL,
			Ref:        headSHA,
			BaseRef:    baseSHA,
			BaseBranch: baseBranch,
			HeadBranch: headBranch,
			FetchRefs:  fetchRefs,
			CloneToken: cloneToken,
		},
		Changeset: &domain.ChangesetSpec{
			Provider:       "github",
			RepoOwner:      owner,
			RepoName:       repo,
			PRNumber:       prNumber,
			PRURL:          pr.GetHTMLURL(),
			Title:          pr.GetTitle(),
			Description:    pr.GetBody(),
			Author:         pr.GetUser().GetLogin(),
			TriggerEvent:   triggerEvent,
			InstallationID: installationID,
			BaseBranch:     baseBranch,
			BaseSHA:        baseSHA,
			HeadBranch:     headBranch,
			HeadSHA:        headSHA,
			UnifiedDiff:    unifiedDiff,
			ChangedFiles:   filesCopy,
		},
		Variables: map[string]any{
			"repo_owner": owner,
			"repo_name":  repo,
			"pr_number":  prNumber,
			"base_sha":   baseSHA,
			"head_sha":   headSHA,
		},
		SinkMetadata: map[string]any{
			"owner":           owner,
			"repo":            repo,
			"pr_number":       prNumber,
			"installation_id": installationID,
			"base_sha":        baseSHA,
			"head_sha":        headSHA,
			"pr_url":          pr.GetHTMLURL(),
			"is_fork":         isFork,
		},
	}
}

// NewRepoOverrideLoader returns an orchestrator.RepoOverrideLoader that securely fetches
// .mochi/config.yaml and any referenced .mochi/ prompt files from the repository's BASE branch/SHA
// (never from the untrusted PR head branch, per §9.3).
func NewRepoOverrideLoader(clients ClientFactory, allowCustomPrompts bool) orchestrator.RepoOverrideLoader {
	return func(ctx context.Context, task *domain.TaskContext, _ sandbox.Sandbox, overridePath string) (*orchestrator.RepoOverrideBundle, error) {
		if clients == nil || task == nil || task.Changeset == nil {
			return nil, nil
		}
		cs := task.Changeset
		if cs.Provider != "github" || cs.RepoOwner == "" || cs.RepoName == "" {
			return nil, nil
		}
		baseRef := strings.TrimSpace(cs.BaseSHA)
		if baseRef == "" {
			baseRef = strings.TrimSpace(cs.BaseBranch)
		}
		if baseRef == "" {
			return nil, nil
		}

		client, err := clients.ClientForInstallation(ctx, cs.InstallationID)
		if err != nil {
			return nil, fmt.Errorf("repo override github client: %w", err)
		}

		cfgBytes, found, err := fetchRepoFileAtRef(ctx, client, cs.RepoOwner, cs.RepoName, overridePath, baseRef)
		if err != nil {
			return nil, fmt.Errorf("fetch %q from base ref %q: %w", overridePath, baseRef, err)
		}
		if !found {
			return nil, nil
		}

		bundle := &orchestrator.RepoOverrideBundle{
			ConfigYAML: cfgBytes,
		}

		if allowCustomPrompts {
			promptPaths := extractRepoPromptPaths(cfgBytes)
			if len(promptPaths) > 0 {
				tmpDir, err := os.MkdirTemp("", "mochi-repo-prompts-*")
				if err != nil {
					return nil, fmt.Errorf("create temp prompt dir: %w", err)
				}
				cleanup := true
				defer func() {
					if cleanup {
						_ = os.RemoveAll(tmpDir)
					}
				}()
				for _, relPath := range promptPaths {
					if !isSafeMochiPath(relPath) {
						continue
					}
					content, ok, err := fetchRepoFileAtRef(ctx, client, cs.RepoOwner, cs.RepoName, relPath, baseRef)
					if err != nil {
						return nil, fmt.Errorf("fetch repo prompt %q from base ref %q: %w", relPath, baseRef, err)
					}
					if !ok {
						return nil, fmt.Errorf("repo override references missing prompt file %q on base ref %q", relPath, baseRef)
					}
					targetFile := filepath.Join(tmpDir, filepath.FromSlash(relPath))
					if err := os.MkdirAll(filepath.Dir(targetFile), 0o755); err != nil {
						return nil, fmt.Errorf("create repo prompt directory: %w", err)
					}
					if err := os.WriteFile(targetFile, content, 0o600); err != nil {
						return nil, fmt.Errorf("write cached repo prompt %q: %w", relPath, err)
					}
				}
				cleanup = false
				bundle.PromptDir = tmpDir
			}
		}

		return bundle, nil
	}
}

func listRepoDirAtRef(ctx context.Context, client *github.Client, owner, repo, path, ref string) ([]*github.RepositoryContent, error) {
	opts := &github.RepositoryContentGetOptions{Ref: ref}
	_, dirContents, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		var ghErr *github.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	var workflows []*github.RepositoryContent
	for _, entry := range dirContents {
		if entry == nil || entry.GetType() != "file" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(entry.GetName()))
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			workflows = append(workflows, entry)
		}
	}
	sort.Slice(workflows, func(i, j int) bool {
		return workflows[i].GetName() < workflows[j].GetName()
	})
	return workflows, nil
}

func materializeWorkflowRepoPrompts(ctx context.Context, client *github.Client, owner, repo, baseRef string, wf *domain.Workflow) (string, error) {
	if wf == nil {
		return "", nil
	}
	seen := make(map[string]struct{})
	var repoPaths []string
	addPath := func(p string, fromRepo bool) {
		if !fromRepo {
			return
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(p))))
		if !isSafeMochiPath(clean) {
			return
		}
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		repoPaths = append(repoPaths, clean)
	}
	for _, a := range wf.Agents {
		addPath(a.PromptFile, a.PromptFromRepo)
	}
	addPath(wf.Synthesizer.PromptFile, wf.Synthesizer.PromptFromRepo)

	if len(repoPaths) == 0 {
		return "", nil
	}

	tmpDir, err := os.MkdirTemp("", "mochi-repo-wf-prompts-*")
	if err != nil {
		return "", fmt.Errorf("create temp prompt dir: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	for _, relPath := range repoPaths {
		content, ok, err := fetchRepoFileAtRef(ctx, client, owner, repo, relPath, baseRef)
		if err != nil {
			return "", fmt.Errorf("fetch repo prompt %q from base ref %q: %w", relPath, baseRef, err)
		}
		if !ok {
			return "", fmt.Errorf("repo workflow %q references missing prompt file %q on base ref %q", wf.Name, relPath, baseRef)
		}
		targetFile := filepath.Join(tmpDir, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(targetFile), 0o755); err != nil {
			return "", fmt.Errorf("create repo prompt directory: %w", err)
		}
		if err := os.WriteFile(targetFile, content, 0o600); err != nil {
			return "", fmt.Errorf("write cached repo prompt %q: %w", relPath, err)
		}
	}
	cleanup = false
	return tmpDir, nil
}

func fetchRepoFileAtRef(ctx context.Context, client *github.Client, owner, repo, path, ref string) ([]byte, bool, error) {
	opts := &github.RepositoryContentGetOptions{Ref: ref}
	fileContent, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, false, nil
		}
		var ghErr *github.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	if fileContent == nil {
		return nil, false, nil
	}
	decoded, err := fileContent.GetContent()
	if err != nil {
		return nil, false, err
	}
	return []byte(decoded), true, nil
}

func extractRepoPromptPaths(overrideYAML []byte) []string {
	var ov config.RepoOverride
	if err := yaml.UnmarshalWithOptions(overrideYAML, &ov, yaml.Strict()); err != nil {
		return nil
	}
	var paths []string
	for _, a := range ov.Agents {
		if p := strings.TrimSpace(a.PromptFile); p != "" {
			paths = append(paths, p)
		}
	}
	if ov.Synthesizer != nil {
		if p := strings.TrimSpace(ov.Synthesizer.PromptFile); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

func isSafeMochiPath(p string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(p))))
	return strings.HasPrefix(cleaned, ".mochi/") && len(cleaned) > len(".mochi/") && !strings.Contains(cleaned, "..")
}

func shouldApplyWebhookFilters(target *PRTarget) bool {
	if target == nil {
		return false
	}
	if target.Triggers != nil {
		return true
	}
	return strings.HasPrefix(target.TriggerEvent, "pull_request.") ||
		strings.HasPrefix(target.TriggerEvent, "issue_comment.")
}

func (a *Adapter) effectiveTriggers(override *domain.TriggerConfig) domain.TriggerConfig {
	t := a.defaultTriggers
	if override != nil {
		if override.Events != nil {
			t.Events = override.Events
		}
		if len(override.BaseBranches) > 0 {
			t.BaseBranches = override.BaseBranches
		}
		if len(override.HeadBranches) > 0 {
			t.HeadBranches = override.HeadBranches
		}
		if override.CommentCommand != "" {
			t.CommentCommand = override.CommentCommand
		}
		if len(override.AllowedCommentAssociations) > 0 {
			t.AllowedCommentAssociations = override.AllowedCommentAssociations
		}
		t.IgnoreDrafts = override.IgnoreDrafts
	}
	if len(t.AllowedCommentAssociations) == 0 {
		t.AllowedCommentAssociations = defaultCommentAssociations
	}
	return t
}

// MatchBranches reports whether branch matches at least one glob pattern in patterns.
// An empty patterns slice matches all branches.
func MatchBranches(patterns []string, branch string) bool {
	if len(patterns) == 0 {
		return true
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false
	}
	for _, p := range patterns {
		if MatchBranchPattern(p, branch) {
			return true
		}
	}
	return false
}

// MatchBranchPattern matches a git branch name against a glob pattern supporting
// '*' (any non-'/' sequence), '?' (single non-'/' rune), and '**' (any sequence including '/').
func MatchBranchPattern(pattern, branch string) bool {
	p := []rune(strings.TrimSpace(pattern))
	b := []rune(strings.TrimSpace(branch))
	if len(p) == 0 {
		return false
	}
	cols := len(b) + 1
	memo := make([]int8, (len(p)+1)*cols)
	var match func(pi, bi int) bool
	match = func(pi, bi int) bool {
		idx := pi*cols + bi
		if memo[idx] != 0 {
			return memo[idx] == 1
		}
		var ok bool
		switch {
		case pi == len(p):
			ok = bi == len(b)
		case pi+1 < len(p) && p[pi] == '*' && p[pi+1] == '*':
			if match(pi+2, bi) {
				ok = true
			} else if pi+2 < len(p) && p[pi+2] == '/' && match(pi+3, bi) {
				ok = true
			} else if bi < len(b) && match(pi, bi+1) {
				ok = true
			}
		case p[pi] == '*':
			if match(pi+1, bi) || (bi < len(b) && b[bi] != '/' && match(pi, bi+1)) {
				ok = true
			}
		case p[pi] == '?':
			ok = bi < len(b) && b[bi] != '/' && match(pi+1, bi+1)
		default:
			ok = bi < len(b) && p[pi] == b[bi] && match(pi+1, bi+1)
		}
		if ok {
			memo[idx] = 1
		} else {
			memo[idx] = -1
		}
		return ok
	}
	return match(0, 0)
}

func isAllowedCommentAssociation(assoc string, allowed []string) bool {
	if len(allowed) == 0 {
		allowed = defaultCommentAssociations
	}
	norm := strings.ToUpper(strings.TrimSpace(assoc))
	if norm == "" {
		return false
	}
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimSpace(a), norm) {
			return true
		}
	}
	return false
}

func listAllPRFiles(ctx context.Context, client *github.Client, owner, repo string, prNum int) ([]*github.CommitFile, error) {
	var all []*github.CommitFile
	opts := &github.ListOptions{PerPage: 100, Page: 1}
	for {
		files, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, prNum, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, files...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

func extractRepoOwnerName(r *github.Repository) (string, string) {
	if r == nil {
		return "", ""
	}
	owner := r.GetOwner().GetLogin()
	name := r.GetName()
	if (owner == "" || name == "") && r.GetFullName() != "" {
		if o, n, ok := strings.Cut(r.GetFullName(), "/"); ok {
			if owner == "" {
				owner = o
			}
			if name == "" {
				name = n
			}
		}
	}
	return owner, name
}

func matchesCommentCommand(body, cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	for rawLine := range strings.SplitSeq(body, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == cmd || strings.HasPrefix(line, cmd+" ") || strings.HasPrefix(line, cmd+"\t") {
			return true
		}
	}
	return false
}

func normalizeGitHubFileStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "added":
		return "added"
	case "removed", "deleted":
		return "deleted"
	case "renamed":
		return "renamed"
	default:
		return "modified"
	}
}
