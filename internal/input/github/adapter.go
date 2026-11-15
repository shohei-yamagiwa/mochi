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

	defaultPREvents = []string{"opened", "synchronize", "reopened", "ready_for_review"}
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
			Events:         slices.Clone(defaultPREvents),
			CommentCommand: defaultCommentCommand,
			IgnoreDrafts:   true,
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
		if len(allowedEvents) == 0 {
			allowedEvents = defaultPREvents
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
		cmd := strings.TrimSpace(triggers.CommentCommand)
		if cmd == "" {
			return nil, fmt.Errorf("%w: comment_command trigger is disabled", ErrIgnoredEvent)
		}
		if !matchesCommentCommand(ev.GetComment().GetBody(), cmd) {
			return nil, fmt.Errorf("%w: comment does not invoke %q", ErrIgnoredEvent, cmd)
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

	if target.Triggers != nil && target.Triggers.IgnoreDrafts && pr.GetDraft() {
		return nil, fmt.Errorf("%w: pull_request #%d is a draft", ErrIgnoredEvent, target.PRNumber)
	}

	unifiedDiff, _, err := client.PullRequests.GetRaw(ctx, target.Owner, target.Repo, target.PRNumber, github.RawOptions{
		Type: github.Diff,
	})
	if err != nil {
		return nil, fmt.Errorf("get pull request raw diff %s/%s#%d: %w", target.Owner, target.Repo, target.PRNumber, err)
	}

	commitFiles, err := listAllPRFiles(ctx, client, target.Owner, target.Repo, target.PRNumber)
	if err != nil {
		return nil, fmt.Errorf("list pull request files %s/%s#%d: %w", target.Owner, target.Repo, target.PRNumber, err)
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

	cloneToken, err := a.clients.CloneTokenForRepo(ctx, target.InstallationID, target.Repo)
	if err != nil {
		return nil, fmt.Errorf("acquire clone token: %w", err)
	}

	baseBranch := pr.GetBase().GetRef()
	baseSHA := pr.GetBase().GetSHA()
	headBranch := pr.GetHead().GetRef()
	headSHA := pr.GetHead().GetSHA()
	repoURL := ""
	if baseRepo := pr.GetBase().GetRepo(); baseRepo != nil {
		repoURL = baseRepo.GetCloneURL()
	}
	if repoURL == "" {
		repoURL = fmt.Sprintf("https://github.com/%s/%s.git", target.Owner, target.Repo)
	}

	isFork := false
	if headRepo := pr.GetHead().GetRepo(); headRepo != nil {
		isFork = headRepo.GetFullName() != fmt.Sprintf("%s/%s", target.Owner, target.Repo)
	}

	wfName := strings.TrimSpace(target.WorkflowName)
	if wfName == "" {
		wfName = a.workflowName
	}

	dedupKey := fmt.Sprintf("github:%s/%s:pr:%d", target.Owner, target.Repo, target.PRNumber)

	return &domain.TaskContext{
		WorkflowName: wfName,
		TriggerType:  adapterName,
		DedupKey:     dedupKey,
		Workspace: domain.WorkspaceSpec{
			RepoURL:    repoURL,
			Ref:        headSHA,
			BaseRef:    baseSHA,
			CloneToken: cloneToken,
		},
		Changeset: &domain.ChangesetSpec{
			Provider:       "github",
			RepoOwner:      target.Owner,
			RepoName:       target.Repo,
			PRNumber:       target.PRNumber,
			PRURL:          pr.GetHTMLURL(),
			Title:          pr.GetTitle(),
			Description:    pr.GetBody(),
			Author:         pr.GetUser().GetLogin(),
			TriggerEvent:   target.TriggerEvent,
			InstallationID: target.InstallationID,
			BaseBranch:     baseBranch,
			BaseSHA:        baseSHA,
			HeadBranch:     headBranch,
			HeadSHA:        headSHA,
			UnifiedDiff:    unifiedDiff,
			ChangedFiles:   changedFiles,
		},
		Variables: map[string]any{
			"repo_owner": target.Owner,
			"repo_name":  target.Repo,
			"pr_number":  target.PRNumber,
			"base_sha":   baseSHA,
			"head_sha":   headSHA,
		},
		SinkMetadata: map[string]any{
			"owner":           target.Owner,
			"repo":            target.Repo,
			"pr_number":       target.PRNumber,
			"installation_id": target.InstallationID,
			"base_sha":        baseSHA,
			"head_sha":        headSHA,
			"pr_url":          pr.GetHTMLURL(),
			"is_fork":         isFork,
		},
	}, nil
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

func (a *Adapter) effectiveTriggers(override *domain.TriggerConfig) domain.TriggerConfig {
	t := a.defaultTriggers
	if override != nil {
		if len(override.Events) > 0 {
			t.Events = override.Events
		}
		if override.CommentCommand != "" {
			t.CommentCommand = override.CommentCommand
		}
		t.IgnoreDrafts = override.IgnoreDrafts
	}
	return t
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
