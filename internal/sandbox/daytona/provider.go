package daytona

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	posixpath "path"
	"strconv"
	"strings"
	"time"

	daytonasdk "github.com/daytona/clients/sdk-go/pkg/daytona"
	sdkerrors "github.com/daytona/clients/sdk-go/pkg/errors"
	"github.com/daytona/clients/sdk-go/pkg/options"
	"github.com/daytona/clients/sdk-go/pkg/types"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
)

const (
	defaultSetupTimeout    = 3 * time.Minute
	defaultForkTimeout     = 2 * time.Minute
	defaultDestroyTimeout  = 30 * time.Second
	defaultAutoStopMinutes = 15
	defaultRepoSubdir      = "repo"
)

// Config configures the Daytona sandbox provider.
type Config struct {
	APIKey               string
	JWTToken             string
	OrganizationID       string
	APIUrl               string
	Target               string
	HTTPClient           *http.Client
	UseDeprecatedPolling *bool
	SetupTimeout         time.Duration
	ForkTimeout          time.Duration
	DestroyTimeout       time.Duration
	AutoStopMinutes      int
}

// Provider implements sandbox.Provider backed by the official Daytona Go SDK.
type Provider struct {
	client *daytonasdk.Client
	cfg    Config
}

// NewProvider creates a Daytona Provider using explicit Config (falling back to DAYTONA_* env vars).
func NewProvider(cfg Config) (*Provider, error) {
	if cfg.SetupTimeout <= 0 {
		cfg.SetupTimeout = defaultSetupTimeout
	}
	if cfg.ForkTimeout <= 0 {
		cfg.ForkTimeout = defaultForkTimeout
	}
	if cfg.DestroyTimeout <= 0 {
		cfg.DestroyTimeout = defaultDestroyTimeout
	}
	if cfg.AutoStopMinutes <= 0 {
		cfg.AutoStopMinutes = defaultAutoStopMinutes
	}

	sdkCfg := &types.DaytonaConfig{
		APIKey:               cfg.APIKey,
		JWTToken:             cfg.JWTToken,
		OrganizationID:       cfg.OrganizationID,
		APIUrl:               cfg.APIUrl,
		Target:               cfg.Target,
		HTTPClient:           cfg.HTTPClient,
		UseDeprecatedPolling: cfg.UseDeprecatedPolling,
	}
	client, err := daytonasdk.NewClientWithConfig(sdkCfg)
	if err != nil {
		return nil, fmt.Errorf("daytona new client: %w", err)
	}

	return &Provider{
		client: client,
		cfg:    cfg,
	}, nil
}

// CreateAndPrepareBaseSandbox provisions a base Daytona sandbox, clones the target repository,
// checks out opts.Workspace.Ref, and strips ephemeral git credentials before returning it for Fork().
func (p *Provider) CreateAndPrepareBaseSandbox(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	if opts.Workspace.Snapshot == "" && opts.Workspace.Image == "" {
		return nil, fmt.Errorf("daytona: either workspace.snapshot or workspace.image must be specified")
	}

	autoStop := p.cfg.AutoStopMinutes
	baseParams := types.SandboxBaseParams{
		Name:             opts.Name,
		EnvVars:          opts.EnvVars,
		Ephemeral:        true,
		AutoStopInterval: &autoStop,
	}

	var createParams any
	if opts.Workspace.Snapshot != "" {
		createParams = types.SnapshotParams{
			SandboxBaseParams: baseParams,
			Snapshot:          opts.Workspace.Snapshot,
		}
	} else {
		var resources *types.Resources
		if opts.ResourceLimits.CPU > 0 || opts.ResourceLimits.Memory > 0 || opts.ResourceLimits.Disk > 0 {
			resources = &types.Resources{
				CPU:    opts.ResourceLimits.CPU,
				Memory: opts.ResourceLimits.Memory,
				Disk:   opts.ResourceLimits.Disk,
			}
		}
		createParams = types.ImageParams{
			SandboxBaseParams: baseParams,
			Image:             opts.Workspace.Image,
			Resources:         resources,
		}
	}

	rawSB, err := p.client.Create(
		ctx,
		createParams,
		options.WithWaitForStart(true),
		options.WithTimeout(p.cfg.SetupTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("daytona create sandbox: %w", err)
	}

	sb := &Sandbox{
		client:         p.client,
		sb:             rawSB,
		forkTimeout:    p.cfg.ForkTimeout,
		destroyTimeout: p.cfg.DestroyTimeout,
	}

	// Ensure base sandbox is cleaned up if any subsequent preparation step fails.
	cleanupOnErr := true
	defer func() {
		if cleanupOnErr {
			_ = sb.Destroy(ctx)
		}
	}()

	workDir, err := rawSB.GetWorkingDir(ctx)
	if err != nil || strings.TrimSpace(workDir) == "" {
		workDir = "/home/daytona"
	}
	repoDir := posixpath.Join(posixpath.Clean(workDir), defaultRepoSubdir)
	sb.repoDir = repoDir

	if strings.TrimSpace(opts.Workspace.RepoURL) != "" {
		if err := p.cloneAndSanitizeRepo(ctx, rawSB, repoDir, opts.Workspace); err != nil {
			return nil, err
		}
	}

	cleanupOnErr = false
	return sb, nil
}

func (p *Provider) cloneAndSanitizeRepo(ctx context.Context, rawSB *daytonasdk.Sandbox, repoDir string, ws domain.WorkspaceSpec) error {
	var cloneOpts []func(*options.GitClone)
	if ws.CloneToken != "" {
		cloneOpts = append(cloneOpts,
			options.WithUsername("x-access-token"),
			options.WithPassword(ws.CloneToken),
		)
	}
	if ref := strings.TrimSpace(ws.Ref); ref != "" {
		if isLikelyCommitSHA(ref) {
			cloneOpts = append(cloneOpts, options.WithCommitId(ref))
		} else {
			cloneOpts = append(cloneOpts, options.WithBranch(ref))
		}
	}

	if err := rawSB.Git.Clone(ctx, ws.RepoURL, repoDir, cloneOpts...); err != nil {
		return fmt.Errorf("daytona git clone: %s", redactToken(err.Error(), ws.CloneToken))
	}

	// Security invariant (§9.2): strip any embedded credentials / credential helpers
	// from .git/config and sandbox home before Fork() is ever called.
	sanitizedURL := sanitizeRepoURL(ws.RepoURL)
	stripCmd := fmt.Sprintf(
		"git remote set-url origin %s && "+
			"{ git config --local --unset-all credential.helper || true; } && "+
			"{ git config --local --unset-all http.extraheader || true; } && "+
			"rm -f ~/.git-credentials && "+
			`test "$(git config --local --get remote.origin.url)" = %s`,
		shellQuote(sanitizedURL),
		shellQuote(sanitizedURL),
	)
	res, err := rawSB.Process.ExecuteCommand(ctx, stripCmd, options.WithCwd(repoDir))
	if err != nil {
		return fmt.Errorf("daytona strip git credentials: %s", redactToken(err.Error(), ws.CloneToken))
	}
	if res != nil && res.ExitCode != 0 {
		return fmt.Errorf("daytona strip git credentials failed (exit=%d): %s", res.ExitCode, redactToken(res.Result, ws.CloneToken))
	}

	return nil
}

// Sandbox wraps a Daytona SDK Sandbox and implements sandbox.Sandbox.
type Sandbox struct {
	client         *daytonasdk.Client
	sb             *daytonasdk.Sandbox
	repoDir        string
	forkTimeout    time.Duration
	destroyTimeout time.Duration
}

// ID returns the Daytona sandbox identifier.
func (s *Sandbox) ID() string {
	return s.sb.ID
}

// RepoDir returns the absolute path of the cloned repository inside the sandbox.
func (s *Sandbox) RepoDir() string {
	return s.repoDir
}

// Fork clones the current Sandbox state into a new isolated Sandbox VM.
func (s *Sandbox) Fork(ctx context.Context, name string) (sandbox.Sandbox, error) {
	var namePtr *string
	if strings.TrimSpace(name) != "" {
		n := strings.TrimSpace(name)
		namePtr = &n
	}
	forked, err := s.sb.ForkWithTimeout(ctx, namePtr, s.forkTimeout)
	if err != nil {
		return nil, fmt.Errorf("daytona fork sandbox %q: %w", s.sb.ID, err)
	}

	// Server-side fail-safe: ensure forked sandbox deletes immediately upon stop.
	zero := 0
	if err := forked.SetAutoDeleteInterval(ctx, &zero); err != nil {
		_ = forked.Delete(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("daytona set auto-delete on fork %q: %w", forked.ID, err)
	}

	return &Sandbox{
		client:         s.client,
		sb:             forked,
		repoDir:        s.repoDir,
		forkTimeout:    s.forkTimeout,
		destroyTimeout: s.destroyTimeout,
	}, nil
}

// Exec runs a command via Daytona Process.ExecuteCommand.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	workDir := s.repoDir
	if strings.TrimSpace(req.WorkDir) != "" {
		resolved, err := s.resolvePath(req.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("daytona exec invalid workdir: %w", err)
		}
		workDir = resolved
	}

	var execOpts []func(*options.ExecuteCommand)
	if workDir != "" {
		execOpts = append(execOpts, options.WithCwd(workDir))
	}
	if req.Timeout > 0 {
		execOpts = append(execOpts, options.WithExecuteTimeout(req.Timeout))
	}

	resp, err := s.sb.Process.ExecuteCommand(ctx, req.Command, execOpts...)
	if err != nil {
		if isTimeoutErr(ctx, err) {
			return &sandbox.ExecResult{
				ExitCode: -1,
				Stderr:   err.Error(),
				TimedOut: true,
			}, nil
		}
		return nil, fmt.Errorf("daytona exec: %w", err)
	}

	stdout := resp.Result
	if stdout == "" && resp.Artifacts != nil {
		stdout = resp.Artifacts.Stdout
	}
	return &sandbox.ExecResult{
		ExitCode: resp.ExitCode,
		Stdout:   stdout,
	}, nil
}

// ReadFile downloads a file from the sandbox and optionally slices 1-indexed lines [startLine, endLine].
func (s *Sandbox) ReadFile(ctx context.Context, path string, startLine, endLine int) (string, error) {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return "", fmt.Errorf("daytona read_file: %w", err)
	}
	data, err := s.sb.FileSystem.DownloadFile(ctx, absPath, nil)
	if err != nil {
		return "", fmt.Errorf("daytona download %q: %w", path, err)
	}
	return sliceLines(string(data), startLine, endLine), nil
}

// WriteFile uploads content to path inside the sandbox, creating parent folders if needed.
func (s *Sandbox) WriteFile(ctx context.Context, path string, content []byte) error {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return fmt.Errorf("daytona write_file: %w", err)
	}
	parent := posixpath.Dir(absPath)
	if parent != "" && parent != "/" && parent != "." && parent != s.repoDir {
		_ = s.sb.FileSystem.CreateFolder(ctx, parent)
	}
	if err := s.sb.FileSystem.UploadFile(ctx, content, absPath); err != nil {
		return fmt.Errorf("daytona upload %q: %w", path, err)
	}
	return nil
}

// ListDir lists entries inside path (relative to repoDir or within it).
func (s *Sandbox) ListDir(ctx context.Context, path string) ([]sandbox.FileEntry, error) {
	absPath, err := s.resolvePath(path)
	if err != nil {
		return nil, fmt.Errorf("daytona list_dir: %w", err)
	}
	files, err := s.sb.FileSystem.ListFiles(ctx, absPath)
	if err != nil {
		return nil, fmt.Errorf("daytona list %q: %w", path, err)
	}

	entries := make([]sandbox.FileEntry, 0, len(files))
	for _, f := range files {
		if f == nil {
			continue
		}
		p := f.Path
		if p == "" {
			p = posixpath.Join(absPath, f.Name)
		}
		entries = append(entries, sandbox.FileEntry{
			Name:  f.Name,
			Path:  s.toRepoRelative(p),
			IsDir: f.IsDirectory,
			Size:  f.Size,
		})
	}
	return entries, nil
}

// SearchFiles searches for pattern inside rootPath using Daytona FileSystem.FindFiles.
func (s *Sandbox) SearchFiles(ctx context.Context, rootPath, pattern string) ([]sandbox.SearchMatch, error) {
	absRoot, err := s.resolvePath(rootPath)
	if err != nil {
		return nil, fmt.Errorf("daytona search_files: %w", err)
	}
	raw, err := s.sb.FileSystem.FindFiles(ctx, absRoot, pattern)
	if err != nil {
		return nil, fmt.Errorf("daytona find files %q: %w", rootPath, err)
	}

	rawMatches, ok := raw.([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("daytona find files: unexpected result type %T", raw)
	}

	matches := make([]sandbox.SearchMatch, 0, len(rawMatches))
	for _, m := range rawMatches {
		fileStr, _ := m["file"].(string)
		lineText, _ := m["content"].(string)
		lineNum := toInt(m["line"])
		matches = append(matches, sandbox.SearchMatch{
			File:       s.toRepoRelative(fileStr),
			LineNumber: lineNum,
			LineText:   lineText,
		})
	}
	return matches, nil
}

// Destroy tears down the Daytona sandbox using a detached cleanup context so cancellation/timeout never leaks VMs.
func (s *Sandbox) Destroy(ctx context.Context) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.destroyTimeout)
	defer cancel()

	if err := s.sb.DeleteWithTimeout(cleanupCtx, s.destroyTimeout); err != nil {
		return fmt.Errorf("daytona delete sandbox %q: %w", s.sb.ID, err)
	}
	return nil
}

func (s *Sandbox) resolvePath(p string) (string, error) {
	cleanedInput := strings.TrimSpace(strings.ReplaceAll(p, `\`, "/"))
	root := posixpath.Clean(s.repoDir)
	if root == "" || root == "." {
		if cleanedInput == "" {
			return ".", nil
		}
		return posixpath.Clean(cleanedInput), nil
	}

	if cleanedInput == "" || cleanedInput == "." {
		return root, nil
	}

	var target string
	if strings.HasPrefix(cleanedInput, "/") {
		target = posixpath.Clean(cleanedInput)
	} else {
		target = posixpath.Clean(posixpath.Join(root, cleanedInput))
	}

	if target != root && !strings.HasPrefix(target, root+"/") {
		return "", fmt.Errorf("path %q escapes repository root %q", p, root)
	}
	return target, nil
}

func (s *Sandbox) toRepoRelative(absPath string) string {
	cleaned := posixpath.Clean(strings.ReplaceAll(absPath, `\`, "/"))
	root := posixpath.Clean(s.repoDir)
	if root == "" || root == "." {
		return cleaned
	}
	if cleaned == root {
		return "."
	}
	prefix := root + "/"
	if strings.HasPrefix(cleaned, prefix) {
		return strings.TrimPrefix(cleaned, prefix)
	}
	return cleaned
}

func sliceLines(content string, startLine, endLine int) string {
	if startLine <= 0 && endLine <= 0 {
		return content
	}
	// Preserve consistent line splitting; drop trailing empty element from final newline only when counting lines?
	// Standard 1-indexed line slicing:
	lines := strings.Split(content, "\n")
	if startLine <= 0 {
		startLine = 1
	}
	if startLine > len(lines) {
		return ""
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if endLine < startLine {
		return ""
	}
	return strings.Join(lines[startLine-1:endLine], "\n")
}

func sanitizeRepoURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return rawURL
	}
	if parsed.User != nil {
		parsed.User = nil
	}
	return parsed.String()
}

func redactToken(msg, token string) string {
	if token == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[REDACTED]")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isLikelyCommitSHA(ref string) bool {
	if len(ref) < 7 || len(ref) > 64 {
		return false
	}
	for _, r := range ref {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func isTimeoutErr(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, sdkerrors.ErrProcessExecutionTimeout) ||
		errors.Is(err, sdkerrors.ErrTimeout) ||
		errors.Is(err, sdkerrors.ErrGatewayTimeout) {
		return true
	}
	var daytonaErr *sdkerrors.DaytonaError
	if errors.As(err, &daytonaErr) && (daytonaErr.StatusCode == http.StatusRequestTimeout || daytonaErr.StatusCode == http.StatusGatewayTimeout) {
		return true
	}
	var timeoutErr *sdkerrors.DaytonaTimeoutError
	return errors.As(err, &timeoutErr)
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case float32:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		return 0
	}
}
