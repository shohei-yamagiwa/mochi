package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/input"
	ghinput "github.com/shohei-yamagiwa/mochi/internal/input/github"
)

const adapterName = "cli"

var (
	prURLRegex   = regexp.MustCompile(`^https?://[^/]+/([^/]+)/([^/]+)/pull/(\d+)(?:/.*)?$`)
	prShortRegex = regexp.MustCompile(`^([^/\s]+)/([^#\s]+)#(\d+)$`)
)

// RunRequest configures how the CLI InputAdapter constructs a TaskContext from either
// a GitHub PR reference or a local git repository diff.
type RunRequest struct {
	WorkflowName       string
	RepoPath           string
	RepoURL            string
	BaseRef            string
	HeadRef            string
	PRRef              string
	InstallationID     int64
	IncludeUncommitted bool
	GitHubClients      ghinput.ClientFactory
}

// Adapter implements domain.InputAdapter for `mochi run`.
type Adapter struct{}

var _ domain.InputAdapter = (*Adapter)(nil)

// NewAdapter constructs a CLI InputAdapter.
func NewAdapter() *Adapter {
	return &Adapter{}
}

// Name returns "cli".
func (a *Adapter) Name() string {
	return adapterName
}

// ParsePRRef parses either a GitHub PR URL (https://github.com/owner/repo/pull/42)
// or shorthand (owner/repo#42).
func ParsePRRef(raw string) (owner, repo string, prNumber int, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", 0, false
	}
	if m := prURLRegex.FindStringSubmatch(trimmed); len(m) == 4 {
		n, err := strconv.Atoi(m[3])
		if err != nil || n <= 0 {
			return "", "", 0, false
		}
		return m[1], m[2], n, true
	}
	if m := prShortRegex.FindStringSubmatch(trimmed); len(m) == 4 {
		n, err := strconv.Atoi(m[3])
		if err != nil || n <= 0 {
			return "", "", 0, false
		}
		return m[1], m[2], n, true
	}
	return "", "", 0, false
}

// BuildTaskContext builds a canonical TaskContext from *RunRequest (or RunRequest).
func (a *Adapter) BuildTaskContext(ctx context.Context, rawEvent any) (*domain.TaskContext, error) {
	var req RunRequest
	switch v := rawEvent.(type) {
	case *RunRequest:
		if v == nil {
			return nil, fmt.Errorf("cli input adapter: request must not be nil")
		}
		req = *v
	case RunRequest:
		req = v
	default:
		return nil, fmt.Errorf("cli input adapter: unsupported raw event type %T", rawEvent)
	}

	if strings.TrimSpace(req.PRRef) != "" {
		return a.buildFromPR(ctx, req)
	}
	return a.buildFromLocalGit(ctx, req)
}

func (a *Adapter) buildFromPR(ctx context.Context, req RunRequest) (*domain.TaskContext, error) {
	owner, repo, prNum, ok := ParsePRRef(req.PRRef)
	if !ok {
		return nil, fmt.Errorf("cli input adapter: invalid PR reference %q (expected https://github.com/owner/repo/pull/N or owner/repo#N)", req.PRRef)
	}
	if req.GitHubClients == nil {
		return nil, fmt.Errorf("cli input adapter: GitHub client factory is required for PR references (set GITHUB_TOKEN or GitHub App credentials)")
	}

	ghAdapter, err := ghinput.NewAdapter(req.GitHubClients, ghinput.WithWorkflowName(req.WorkflowName))
	if err != nil {
		return nil, err
	}
	task, err := ghAdapter.BuildTaskContext(ctx, &ghinput.PRTarget{
		Owner:          owner,
		Repo:           repo,
		PRNumber:       prNum,
		InstallationID: req.InstallationID,
		TriggerEvent:   "cli.pr",
		WorkflowName:   req.WorkflowName,
	})
	if err != nil {
		return nil, err
	}
	task.TriggerType = adapterName
	return task, nil
}

func (a *Adapter) buildFromLocalGit(ctx context.Context, req RunRequest) (*domain.TaskContext, error) {
	repoPath := strings.TrimSpace(req.RepoPath)
	if repoPath == "" {
		repoPath = "."
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path %q: %w", repoPath, err)
	}

	topLevel, err := runGit(ctx, absRepo, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("cli input adapter: %q is not a git repository: %w", absRepo, err)
	}
	absRepo = strings.TrimSpace(topLevel)

	headRef := strings.TrimSpace(req.HeadRef)
	if headRef == "" {
		headRef = "HEAD"
	}
	headSHA, err := runGit(ctx, absRepo, "rev-parse", headRef)
	if err != nil {
		return nil, fmt.Errorf("resolve head ref %q: %w", headRef, err)
	}
	headSHA = strings.TrimSpace(headSHA)

	headBranch, _ := runGit(ctx, absRepo, "rev-parse", "--abbrev-ref", headRef)
	headBranch = strings.TrimSpace(headBranch)

	baseRef := strings.TrimSpace(req.BaseRef)
	if baseRef == "" {
		if _, err := runGit(ctx, absRepo, "rev-parse", "--verify", "HEAD~1"); err == nil {
			baseRef = "HEAD~1"
		} else {
			// Single-commit repository: compare against empty tree.
			baseRef = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
		}
	}

	baseSHA := baseRef
	if resolved, err := runGit(ctx, absRepo, "rev-parse", baseRef); err == nil {
		baseSHA = strings.TrimSpace(resolved)
	}

	var diffOutput string
	if req.IncludeUncommitted {
		diffOutput, err = runGit(ctx, absRepo, "diff", "--unified=3", baseSHA)
	} else {
		diffOutput, err = runGit(ctx, absRepo, "diff", "--unified=3", baseSHA+".."+headSHA)
	}
	if err != nil {
		return nil, fmt.Errorf("compute git diff %s..%s: %w", baseSHA, headSHA, err)
	}

	commitSubject, _ := runGit(ctx, absRepo, "log", "-1", "--pretty=%s", headSHA)
	commitBody, _ := runGit(ctx, absRepo, "log", "-1", "--pretty=%b", headSHA)
	commitAuthor, _ := runGit(ctx, absRepo, "log", "-1", "--pretty=%an", headSHA)

	repoURL := strings.TrimSpace(req.RepoURL)
	if repoURL == "" {
		if originURL, err := runGit(ctx, absRepo, "remote", "get-url", "origin"); err == nil {
			repoURL = sanitizeRemoteURL(strings.TrimSpace(originURL))
		}
	}
	if repoURL == "" {
		repoURL = absRepo
	}

	owner, name := parseRepoOwnerName(repoURL, filepath.Base(absRepo))
	changedFiles := input.ParseUnifiedDiff(diffOutput)

	dedupKey := fmt.Sprintf("cli:%s/%s:%s..%s", owner, name, shortSHA(baseSHA), shortSHA(headSHA))

	return &domain.TaskContext{
		WorkflowName: strings.TrimSpace(req.WorkflowName),
		TriggerType:  adapterName,
		DedupKey:     dedupKey,
		Workspace: domain.WorkspaceSpec{
			RepoURL: repoURL,
			Ref:     headSHA,
			BaseRef: baseSHA,
		},
		Changeset: &domain.ChangesetSpec{
			Provider:     "git_local",
			RepoOwner:    owner,
			RepoName:     name,
			Title:        strings.TrimSpace(commitSubject),
			Description:  strings.TrimSpace(commitBody),
			Author:       strings.TrimSpace(commitAuthor),
			TriggerEvent: "cli.local_diff",
			BaseBranch:   baseRef,
			BaseSHA:      baseSHA,
			HeadBranch:   headBranch,
			HeadSHA:      headSHA,
			UnifiedDiff:  diffOutput,
			ChangedFiles: changedFiles,
		},
		Variables: map[string]any{
			"repo_path":  absRepo,
			"repo_owner": owner,
			"repo_name":  name,
			"base_sha":   baseSHA,
			"head_sha":   headSHA,
		},
	}, nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			return "", err
		}
		return "", errors.New(errMsg)
	}
	return stdout.String(), nil
}

func sanitizeRemoteURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" {
		u.User = nil
		return u.String()
	}
	return raw
}

func parseRepoOwnerName(repoURL, fallbackName string) (string, string) {
	trimmed := strings.TrimSpace(repoURL)
	trimmed = strings.TrimSuffix(trimmed, ".git")

	// Handle scp-like git@github.com:owner/repo
	if idx := strings.Index(trimmed, ":"); idx > 0 && !strings.Contains(trimmed, "://") {
		pathPart := strings.TrimLeft(trimmed[idx+1:], "/")
		parts := strings.Split(pathPart, "/")
		if len(parts) >= 2 {
			return parts[len(parts)-2], parts[len(parts)-1]
		}
	}

	if u, err := url.Parse(trimmed); err == nil && u.Path != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 2 {
			return parts[len(parts)-2], parts[len(parts)-1]
		}
	}

	return "local", fallbackName
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
