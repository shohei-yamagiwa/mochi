package daytona

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		mu             sync.Mutex
		reqs           []recordedReq
		uploadedBytes  []byte
		uploadedPath   string
		cloneFailToken bool
		execTimeout    bool
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
			if strings.Contains(cmd, "git remote set-url origin 'https://github.com/org/repo.git'") &&
				strings.Contains(cmd, "credential.helper") &&
				strings.Contains(cmd, "http.extraheader") {
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

	// 3. Exec normal + relative WorkDir + timeout
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
