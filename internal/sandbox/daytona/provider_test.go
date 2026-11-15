package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
)

type recordedReq struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func TestDaytonaProvider_CreatePrepareForkExecFilesAndDestroy(t *testing.T) {
	var (
		mu                      sync.Mutex
		reqs                    []recordedReq
		uploadedBytes           []byte
		uploadedPath            string
		cloneFailToken          bool
		execTimeout             bool
		execFailWithTimeoutText bool
	)

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recordedReq{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Body:   string(raw),
		})
		failClone := cloneFailToken
		timeoutExec := execTimeout
		failExecMsg := execFailWithTimeoutText
		mu.Unlock()

		writeJSON := func(status int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}

		sandboxPayload := func(id, name string, autoDelete int) map[string]any {
			return map[string]any{
				"id":                  id,
				"organizationId":      "org-1",
				"name":                name,
				"user":                "daytona",
				"env":                 map[string]string{},
				"labels":              map[string]string{"code-toolbox-language": "python"},
				"public":              false,
				"networkBlockAll":     false,
				"kvm":                 false,
				"target":              "us",
				"cpu":                 2,
				"gpu":                 0,
				"memory":              4,
				"disk":                20,
				"state":               "started",
				"toolboxProxyUrl":     srvURL + "/toolbox",
				"autoStopInterval":    15,
				"autoArchiveInterval": 15,
				"autoDeleteInterval":  autoDelete,
			}
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox":
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			name, _ := body["name"].(string)
			writeJSON(http.StatusOK, sandboxPayload("sb-base", name, 0))

		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/sb-base/fork":
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			name, _ := body["name"].(string)
			writeJSON(http.StatusOK, sandboxPayload("sb-fork-1", name, -1))

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sandbox/") && strings.Contains(r.URL.Path, "/autodelete/"):
			id := strings.Split(strings.TrimPrefix(r.URL.Path, "/sandbox/"), "/")[0]
			writeJSON(http.StatusOK, sandboxPayload(id, id, 0))

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandbox/"):
			id := strings.TrimPrefix(r.URL.Path, "/sandbox/")
			writeJSON(http.StatusOK, sandboxPayload(id, id, 0))

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work-dir"):
			writeJSON(http.StatusOK, map[string]any{"dir": "/home/daytona"})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/clone"):
			if failClone {
				writeJSON(http.StatusInternalServerError, map[string]any{
					"message": "auth failed for https://x-access-token:ghs_super_secret@github.com/org/repo.git",
				})
				return
			}
			writeJSON(http.StatusOK, map[string]any{})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/process/execute"):
			if timeoutExec {
				writeJSON(http.StatusGatewayTimeout, map[string]any{"message": "command execution timeout"})
				return
			}
			if failExecMsg {
				writeJSON(http.StatusInternalServerError, map[string]any{"message": "invalid toolbox timeout config"})
				return
			}
			writeJSON(http.StatusOK, map[string]any{
				"exitCode": 0,
				"result":   "hello stdout",
			})

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files/download"):
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("line1\nline2\nline3\nline4"))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files/folder"):
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files/upload-v2"):
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			// Reconstruct body from raw since we read it earlier
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatalf("ParseMultipartForm: %v", err)
			}
			f, _, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("FormFile(file): %v", err)
			}
			defer f.Close()
			data, _ := io.ReadAll(f)
			mu.Lock()
			uploadedBytes = data
			uploadedPath = r.URL.Query().Get("path")
			mu.Unlock()
			writeJSON(http.StatusOK, map[string]any{
				"name": "new.go",
				"path": r.URL.Query().Get("path"),
				"type": "file",
			})

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files/find"):
			writeJSON(http.StatusOK, []map[string]any{
				{
					"file":    "/home/daytona/repo/internal/auth.go",
					"line":    12,
					"content": "func Auth() error {",
				},
			})

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files"):
			writeJSON(http.StatusOK, []map[string]any{
				{
					"name":        "go.mod",
					"path":        "/home/daytona/repo/go.mod",
					"isDir":       false,
					"size":        120,
					"mode":        "-rw-r--r--",
					"permissions": "0644",
					"owner":       "daytona",
					"group":       "daytona",
					"modTime":     "2026-11-14T00:00:00Z",
					"modifiedAt":  "2026-11-14T00:00:00Z",
				},
				{
					"name":        "internal",
					"path":        "/home/daytona/repo/internal",
					"isDir":       true,
					"size":        4096,
					"mode":        "drwxr-xr-x",
					"permissions": "0755",
					"owner":       "daytona",
					"group":       "daytona",
					"modTime":     "2026-11-14T00:00:00Z",
					"modifiedAt":  "2026-11-14T00:00:00Z",
				},
			})

		default:
			t.Fatalf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	useDeprecated := true
	prov, err := NewProvider(Config{
		APIKey:               "daytona-test-key",
		APIUrl:               srv.URL,
		UseDeprecatedPolling: &useDeprecated,
		SetupTimeout:         5 * time.Second,
		ForkTimeout:          5 * time.Second,
		DestroyTimeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	ctx := context.Background()

	// 1. CreateAndPrepareBaseSandbox with snapshot + branch + CloneToken
	baseSB, err := prov.CreateAndPrepareBaseSandbox(ctx, sandbox.CreateOptions{
		Name: "mochi-base",
		Workspace: domain.WorkspaceSpec{
			RepoURL:    "https://github.com/org/repo.git",
			Ref:        "feat/login",
			CloneToken: "ghs_super_secret",
			Snapshot:   "go-1.24",
		},
		EnvVars: map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("CreateAndPrepareBaseSandbox: %v", err)
	}
	if baseSB.ID() != "sb-base" {
		t.Fatalf("expected sb-base, got %q", baseSB.ID())
	}

	// Verify create payload has ephemeral autoDeleteInterval=0, snapshot, env, and git clone & credential strip ran BEFORE returning
	mu.Lock()
	var sawCreate, sawClone, sawStrip bool
	for _, rq := range reqs {
		if rq.Method == http.MethodPost && rq.Path == "/sandbox" {
			sawCreate = true
			var body map[string]any
			_ = json.Unmarshal([]byte(rq.Body), &body)
			if body["snapshot"] != "go-1.24" {
				t.Fatalf("expected snapshot=go-1.24, got %v", body["snapshot"])
			}
			if int(body["autoDeleteInterval"].(float64)) != 0 {
				t.Fatalf("expected ephemeral autoDeleteInterval=0, got %v", body["autoDeleteInterval"])
			}
		}
		if rq.Method == http.MethodPost && rq.Path == "/toolbox/sb-base/git/clone" {
			sawClone = true
			var body map[string]any
			_ = json.Unmarshal([]byte(rq.Body), &body)
			if body["branch"] != "feat/login" || body["password"] != "ghs_super_secret" || body["path"] != "/home/daytona/repo" {
				t.Fatalf("unexpected git clone body: %v", body)
			}
		}
		if rq.Method == http.MethodPost && rq.Path == "/toolbox/sb-base/process/execute" {
			var body map[string]any
			_ = json.Unmarshal([]byte(rq.Body), &body)
			cmd, _ := body["command"].(string)
			if strings.Contains(cmd, "git remote set-url origin 'https://github.com/org/repo.git' && ") &&
				strings.Contains(cmd, "{ git config --local --unset-all credential.helper || true; } && ") &&
				strings.Contains(cmd, "{ git config --local --unset-all http.extraheader || true; } && ") &&
				strings.Contains(cmd, `test "$(git config --local --get remote.origin.url)" = 'https://github.com/org/repo.git'`) {
				sawStrip = true
			}
		}
	}
	reqs = nil
	mu.Unlock()
	if !sawCreate || !sawClone || !sawStrip {
		t.Fatalf("missing expected preparation calls: create=%v clone=%v strip=%v", sawCreate, sawClone, sawStrip)
	}

	// 2. Fork sets AutoDeleteInterval=0 on forked sandbox
	forkedSB, err := baseSB.Fork(ctx, "mochi-fork-1")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if forkedSB.ID() != "sb-fork-1" {
		t.Fatalf("expected sb-fork-1, got %q", forkedSB.ID())
	}
	mu.Lock()
	var sawFork, sawAutoDel bool
	for _, rq := range reqs {
		if rq.Method == http.MethodPost && rq.Path == "/sandbox/sb-base/fork" {
			sawFork = true
		}
		if rq.Method == http.MethodPost && rq.Path == "/sandbox/sb-fork-1/autodelete/0" {
			sawAutoDel = true
		}
	}
	reqs = nil
	mu.Unlock()
	if !sawFork || !sawAutoDel {
		t.Fatalf("expected fork and autodelete/0 calls: fork=%v autodel=%v", sawFork, sawAutoDel)
	}

	// 3. Exec normal + relative WorkDir + timeout + non-timeout 500 mentioning "timeout"
	res, err := forkedSB.Exec(ctx, sandbox.ExecRequest{
		Command: "go test ./...",
		WorkDir: "internal",
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "hello stdout" || res.TimedOut {
		t.Fatalf("unexpected exec result: %+v", res)
	}
	mu.Lock()
	var execBody map[string]any
	_ = json.Unmarshal([]byte(reqs[0].Body), &execBody)
	if execBody["cwd"] != "/home/daytona/repo/internal" {
		mu.Unlock()
		t.Fatalf("expected cwd=/home/daytona/repo/internal, got %v", execBody["cwd"])
	}
	execTimeout = true
	reqs = nil
	mu.Unlock()

	timedOutRes, err := forkedSB.Exec(ctx, sandbox.ExecRequest{Command: "sleep 60"})
	if err != nil {
		t.Fatalf("Exec timeout should return TimedOut=true, err=nil; got err=%v", err)
	}
	if !timedOutRes.TimedOut || timedOutRes.ExitCode != -1 {
		t.Fatalf("expected TimedOut=true and ExitCode=-1, got %+v", timedOutRes)
	}
	mu.Lock()
	execTimeout = false
	execFailWithTimeoutText = true
	mu.Unlock()

	if nonTimeoutRes, err := forkedSB.Exec(ctx, sandbox.ExecRequest{Command: "echo hi"}); err == nil {
		t.Fatalf("expected HTTP 500 error mentioning timeout to return an error, got %+v", nonTimeoutRes)
	}
	mu.Lock()
	execFailWithTimeoutText = false
	mu.Unlock()

	// 4. ReadFile full, line-sliced, and path-traversal rejection
	full, err := forkedSB.ReadFile(ctx, "README.md", 0, 0)
	if err != nil || full != "line1\nline2\nline3\nline4" {
		t.Fatalf("unexpected ReadFile full: %q, err=%v", full, err)
	}
	sliced, err := forkedSB.ReadFile(ctx, "README.md", 2, 3)
	if err != nil || sliced != "line2\nline3" {
		t.Fatalf("unexpected ReadFile sliced: %q, err=%v", sliced, err)
	}
	if _, err := forkedSB.ReadFile(ctx, "../../etc/passwd", 0, 0); err == nil {
		t.Fatalf("expected path traversal error for ../../etc/passwd")
	}

	// 5. WriteFile
	if err := forkedSB.WriteFile(ctx, "pkg/new.go", []byte("package pkg\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mu.Lock()
	if string(uploadedBytes) != "package pkg\n" || uploadedPath != "/home/daytona/repo/pkg/new.go" {
		mu.Unlock()
		t.Fatalf("unexpected upload: path=%q data=%q", uploadedPath, string(uploadedBytes))
	}
	mu.Unlock()

	// 6. ListDir & SearchFiles
	entries, err := forkedSB.ListDir(ctx, ".")
	if err != nil || len(entries) != 2 || entries[0].Path != "go.mod" || !entries[1].IsDir || entries[1].Path != "internal" {
		t.Fatalf("unexpected ListDir entries: %+v, err=%v", entries, err)
	}
	matches, err := forkedSB.SearchFiles(ctx, ".", "func Auth")
	if err != nil || len(matches) != 1 || matches[0].File != "internal/auth.go" || matches[0].LineNumber != 12 {
		t.Fatalf("unexpected SearchFiles matches: %+v, err=%v", matches, err)
	}

	// 7. Destroy even with already-cancelled context (context.WithoutCancel)
	mu.Lock()
	reqs = nil
	mu.Unlock()
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := forkedSB.Destroy(cancelledCtx); err != nil {
		t.Fatalf("Destroy with cancelled ctx failed: %v", err)
	}
	mu.Lock()
	var sawDelete bool
	for _, rq := range reqs {
		if rq.Method == http.MethodDelete && rq.Path == "/sandbox/sb-fork-1" {
			sawDelete = true
		}
	}
	reqs = nil
	mu.Unlock()
	if !sawDelete {
		t.Fatalf("expected DELETE /sandbox/sb-fork-1 even with cancelled context")
	}

	// 8. Image + ResourceLimits + commit SHA + CloneToken redaction + cleanup on failure
	mu.Lock()
	cloneFailToken = true
	reqs = nil
	mu.Unlock()

	_, err = prov.CreateAndPrepareBaseSandbox(ctx, sandbox.CreateOptions{
		Name: "mochi-img-fail",
		Workspace: domain.WorkspaceSpec{
			RepoURL:    "https://github.com/org/repo.git",
			Ref:        "abcdef1234567890",
			CloneToken: "ghs_super_secret",
			Image:      "golang:1.24",
		},
		ResourceLimits: sandbox.ResourceLimits{CPU: 4, Memory: 8, Disk: 40},
	})
	if err == nil {
		t.Fatalf("expected clone failure error")
	}
	if strings.Contains(err.Error(), "ghs_super_secret") {
		t.Fatalf("CloneToken leaked in error: %v", err)
	}
	mu.Lock()
	var sawCommitID, sawImageCreate, sawCleanupDelete bool
	for _, rq := range reqs {
		if rq.Method == http.MethodPost && rq.Path == "/sandbox" {
			var body map[string]any
			_ = json.Unmarshal([]byte(rq.Body), &body)
			buildInfo, _ := body["buildInfo"].(map[string]any)
			df, _ := buildInfo["dockerfileContent"].(string)
			if strings.Contains(df, "FROM golang:1.24") &&
				int(body["cpu"].(float64)) == 4 &&
				int(body["memory"].(float64)) == 8 &&
				int(body["disk"].(float64)) == 40 {
				sawImageCreate = true
			}
		}
		if rq.Method == http.MethodPost && rq.Path == "/toolbox/sb-base/git/clone" {
			var body map[string]any
			_ = json.Unmarshal([]byte(rq.Body), &body)
			if body["commit_id"] == "abcdef1234567890" {
				sawCommitID = true
			}
		}
		if rq.Method == http.MethodDelete && rq.Path == "/sandbox/sb-base" {
			sawCleanupDelete = true
		}
	}
	mu.Unlock()
	if !sawImageCreate || !sawCommitID || !sawCleanupDelete {
		t.Fatalf("expected image create, commit_id clone, and cleanup delete on failure: image=%v commitId=%v delete=%v", sawImageCreate, sawCommitID, sawCleanupDelete)
	}
}

func TestDaytonaProvider_PRHeadCommitCloneAndFetch(t *testing.T) {
	var (
		mu       sync.Mutex
		cloneReq map[string]any
		execReqs []map[string]any
	)
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                  "sb-pr",
				"organizationId":      "org-1",
				"name":                "mochi-pr",
				"user":                "daytona",
				"env":                 map[string]string{},
				"labels":              map[string]string{"code-toolbox-language": "python"},
				"public":              false,
				"networkBlockAll":     false,
				"kvm":                 false,
				"target":              "us",
				"cpu":                 2,
				"gpu":                 0,
				"memory":              4,
				"disk":                20,
				"state":               "started",
				"toolboxProxyUrl":     srvURL + "/toolbox",
				"autoStopInterval":    15,
				"autoArchiveInterval": 15,
				"autoDeleteInterval":  0,
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work-dir"):
			_ = json.NewEncoder(w).Encode(map[string]any{"dir": "/home/daytona"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/clone"):
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			cloneReq = body
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/process/execute"):
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			execReqs = append(execReqs, body)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "result": ""})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	useDeprecated := true
	prov, err := NewProvider(Config{
		APIKey:               "daytona-test-key",
		APIUrl:               srv.URL,
		UseDeprecatedPolling: &useDeprecated,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	headSHA := "1111111222222233333334444444555555566666"
	baseSHA := "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeee"
	token := "ghs_pr_clone_secret"
	_, err = prov.CreateAndPrepareBaseSandbox(context.Background(), sandbox.CreateOptions{
		Name: "mochi-pr",
		Workspace: domain.WorkspaceSpec{
			RepoURL:    "https://github.com/shohei-yamagiwa/mochi.git",
			Ref:        headSHA,
			BaseRef:    baseSHA,
			BaseBranch: "main",
			HeadBranch: "feat/pr-fix",
			FetchRefs:  []string{"refs/pull/42/head"},
			CloneToken: token,
			Snapshot:   "go-1.24",
		},
	})
	if err != nil {
		t.Fatalf("CreateAndPrepareBaseSandbox: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if cloneReq == nil {
		t.Fatalf("expected git clone request")
	}
	if cloneReq["branch"] != "main" {
		t.Fatalf("expected clone branch=main for PR base branch, got %v", cloneReq["branch"])
	}
	if _, hasCommitID := cloneReq["commit_id"]; hasCommitID && cloneReq["commit_id"] != "" {
		t.Fatalf("did not expect single-branch commit_id on PR clone, got %v", cloneReq["commit_id"])
	}
	if len(execReqs) != 2 {
		t.Fatalf("expected 2 process/execute calls (fetch+checkout, strip), got %d", len(execReqs))
	}

	fetchCmd, _ := execReqs[0]["command"].(string)
	if strings.Contains(fetchCmd, token) {
		t.Fatalf("clone token must not appear in shell command string")
	}
	if !strings.Contains(fetchCmd, "+refs/pull/42/head") || !strings.Contains(fetchCmd, "git checkout --detach '"+headSHA+"'") {
		t.Fatalf("unexpected fetch/checkout command: %s", fetchCmd)
	}
	envsMap, _ := execReqs[0]["envs"].(map[string]any)
	if envsMap == nil || envsMap["MOCHI_CLONE_TOKEN"] != token || envsMap["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("unexpected fetch envs: %v", envsMap)
	}

	stripCmd, _ := execReqs[1]["command"].(string)
	if !strings.Contains(stripCmd, "git remote set-url origin 'https://github.com/shohei-yamagiwa/mochi.git'") {
		t.Fatalf("expected final credential strip command, got: %s", stripCmd)
	}
}

func TestDaytonaProvider_LocalGitPRFetchAndCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found")
	}

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Mochi Test",
			"GIT_AUTHOR_EMAIL=mochi@example.com",
			"GIT_COMMITTER_NAME=Mochi Test",
			"GIT_COMMITTER_EMAIL=mochi@example.com",
		)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, out.String())
		}
		return strings.TrimSpace(out.String())
	}

	tmp := t.TempDir()
	remoteDir := filepath.Join(tmp, "remote.git")
	workSrc := filepath.Join(tmp, "work-src")
	sandboxHome := filepath.Join(tmp, "sandbox-home")
	repoDir := filepath.Join(sandboxHome, "repo")
	if err := os.MkdirAll(sandboxHome, 0o755); err != nil {
		t.Fatal(err)
	}

	runGit(tmp, "init", "--bare", remoteDir)
	runGit(tmp, "clone", remoteDir, workSrc)
	runGit(workSrc, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(workSrc, "app.go"), []byte("package app\n\nfunc Version() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workSrc, "add", "app.go")
	runGit(workSrc, "commit", "-m", "base commit")
	baseSHA := runGit(workSrc, "rev-parse", "HEAD")
	runGit(workSrc, "push", "-u", "origin", "main")
	runGit(remoteDir, "symbolic-ref", "HEAD", "refs/heads/main")

	// Create PR branch commit (only reachable via refs/pull/7/head, simulating a fork PR).
	runGit(workSrc, "checkout", "-b", "fork-pr")
	if err := os.WriteFile(filepath.Join(workSrc, "pr.go"), []byte("package app\n\nfunc PRFeature() bool { return true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workSrc, "add", "pr.go")
	runGit(workSrc, "commit", "-m", "pr commit")
	prHeadSHA := runGit(workSrc, "rev-parse", "HEAD")
	runGit(workSrc, "push", "origin", prHeadSHA+":refs/pull/7/head")

	// Advance main so main has a commit PR branch doesn't have.
	runGit(workSrc, "checkout", "main")
	if err := os.WriteFile(filepath.Join(workSrc, "main_only.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workSrc, "add", "main_only.go")
	runGit(workSrc, "commit", "-m", "newer main commit")
	runGit(workSrc, "push", "origin", "main")

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                  "sb-real-git",
				"organizationId":      "org-1",
				"name":                "mochi-real-git",
				"user":                "daytona",
				"env":                 map[string]string{},
				"labels":              map[string]string{"code-toolbox-language": "python"},
				"public":              false,
				"networkBlockAll":     false,
				"kvm":                 false,
				"target":              "us",
				"cpu":                 2,
				"gpu":                 0,
				"memory":              4,
				"disk":                20,
				"state":               "started",
				"toolboxProxyUrl":     srvURL + "/toolbox",
				"autoStopInterval":    15,
				"autoArchiveInterval": 15,
				"autoDeleteInterval":  0,
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work-dir"):
			_ = json.NewEncoder(w).Encode(map[string]any{"dir": sandboxHome})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/clone"):
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			branch, _ := body["branch"].(string)
			args := []string{"clone", "--single-branch"}
			if branch != "" {
				args = append(args, "--branch", branch)
			}
			args = append(args, remoteDir, repoDir)
			runGit(sandboxHome, args...)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/process/execute"):
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			cmdStr, _ := body["command"].(string)
			cwd, _ := body["cwd"].(string)
			cmd := exec.Command("sh", "-c", cmdStr)
			cmd.Dir = cwd
			cmd.Env = os.Environ()
			if envsRaw, ok := body["envs"].(map[string]any); ok {
				for k, v := range envsRaw {
					if s, ok := v.(string); ok {
						cmd.Env = append(cmd.Env, k+"="+s)
					}
				}
			}
			out, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				exitCode = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"exitCode": exitCode,
				"result":   string(out),
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	useDeprecated := true
	prov, err := NewProvider(Config{
		APIKey:               "daytona-test-key",
		APIUrl:               srv.URL,
		UseDeprecatedPolling: &useDeprecated,
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = prov.CreateAndPrepareBaseSandbox(context.Background(), sandbox.CreateOptions{
		Name: "mochi-real-git",
		Workspace: domain.WorkspaceSpec{
			RepoURL:    remoteDir,
			Ref:        prHeadSHA,
			BaseRef:    baseSHA,
			BaseBranch: "main",
			HeadBranch: "fork-pr", // Deliberately NOT pushed as a branch on remote!
			FetchRefs:  []string{"refs/pull/7/head"},
			Snapshot:   "go-1.24",
		},
	})
	if err != nil {
		t.Fatalf("CreateAndPrepareBaseSandbox failed: %v", err)
	}

	gotHead := runGit(repoDir, "rev-parse", "HEAD")
	if gotHead != prHeadSHA {
		t.Fatalf("checked out HEAD = %s, want %s", gotHead, prHeadSHA)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "pr.go")); err != nil {
		t.Fatalf("expected pr.go in checked out PR workspace: %v", err)
	}
	diffOut := runGit(repoDir, "diff", "--name-only", baseSHA+"...HEAD")
	if diffOut != "pr.go" {
		t.Fatalf("three-dot diff base...HEAD = %q, want %q", diffOut, "pr.go")
	}
}

