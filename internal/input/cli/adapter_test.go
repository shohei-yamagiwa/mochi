package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-github/v93/github"
	"github.com/shohei-yamagiwa/mochi/internal/input/cli"
	ghinput "github.com/shohei-yamagiwa/mochi/internal/input/github"
)

func TestParsePRRef(t *testing.T) {
	cases := []struct {
		raw   string
		owner string
		repo  string
		num   int
		ok    bool
	}{
		{"https://github.com/shohei-yamagiwa/mochi/pull/42", "shohei-yamagiwa", "mochi", 42, true},
		{"https://github.com/shohei-yamagiwa/mochi/pull/42/files", "shohei-yamagiwa", "mochi", 42, true},
		{"shohei-yamagiwa/mochi#7", "shohei-yamagiwa", "mochi", 7, true},
		{"invalid-ref", "", "", 0, false},
		{"shohei-yamagiwa/mochi#0", "", "", 0, false},
	}

	for _, tc := range cases {
		owner, repo, num, ok := cli.ParsePRRef(tc.raw)
		if ok != tc.ok || owner != tc.owner || repo != tc.repo || num != tc.num {
			t.Fatalf("ParsePRRef(%q) = (%q, %q, %d, %v), want (%q, %q, %d, %v)",
				tc.raw, owner, repo, num, ok, tc.owner, tc.repo, tc.num, tc.ok)
		}
	}
}

func TestCLIAdapter_LocalGitRepo(t *testing.T) {
	dir := t.TempDir()
	runGitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@example.com",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}

	runGitCmd("init", "-b", "main")
	runGitCmd("remote", "add", "origin", "https://secret:tok@github.com/shohei-yamagiwa/mochi.git")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd("add", ".")
	runGitCmd("commit", "-m", "initial commit")

	runGitCmd("checkout", "-b", "feat/add-hello")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd("add", ".")
	runGitCmd("commit", "-m", "add hello log")

	adapter := cli.NewAdapter()
	task, err := adapter.BuildTaskContext(context.Background(), &cli.RunRequest{
		WorkflowName: "pr-review",
		RepoPath:     dir,
		BaseRef:      "main",
		HeadRef:      "HEAD",
	})
	if err != nil {
		t.Fatalf("BuildTaskContext: %v", err)
	}

	if task.TriggerType != "cli" {
		t.Fatalf("TriggerType = %q, want cli", task.TriggerType)
	}
	if task.Workspace.RepoURL != "https://github.com/shohei-yamagiwa/mochi.git" {
		t.Fatalf("expected credentials stripped from RepoURL, got %q", task.Workspace.RepoURL)
	}
	if task.Changeset == nil || len(task.Changeset.ChangedFiles) != 1 {
		t.Fatalf("unexpected Changeset: %+v", task.Changeset)
	}
	if task.Changeset.ChangedFiles[0].Path != "main.go" || task.Changeset.Title != "add hello log" {
		t.Fatalf("unexpected ChangedFile/Title: %+v", task.Changeset)
	}
}

func TestCLIAdapter_GitHubPRRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/repo/pulls/12":
			if r.Header.Get("Accept") == "application/vnd.github.v3.diff" {
				_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n package a\n+var x = 1\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   12,
				"title":    "PR from CLI",
				"html_url": "https://github.com/octo/repo/pull/12",
				"user":     map[string]any{"login": "octo"},
				"base":     map[string]any{"ref": "main", "sha": "base123", "repo": map[string]any{"clone_url": "https://github.com/octo/repo.git"}},
				"head":     map[string]any{"ref": "feat", "sha": "head456"},
			})
		case "/repos/octo/repo/pulls/12/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "a.go", "status": "modified", "additions": 1, "patch": "@@ -1 +1,2 @@\n package a\n+var x = 1"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	baseURL := srv.URL + "/"
	ghClient, err := github.NewClient(
		github.WithHTTPClient(srv.Client()),
		github.WithURLs(&baseURL, &baseURL),
	)
	if err != nil {
		t.Fatal(err)
	}

	adapter := cli.NewAdapter()
	task, err := adapter.BuildTaskContext(context.Background(), &cli.RunRequest{
		WorkflowName:  "pr-review",
		PRRef:         "https://github.com/octo/repo/pull/12",
		GitHubClients: ghinput.NewStaticClientFactory(ghClient, "cli-tok"),
	})
	if err != nil {
		t.Fatalf("BuildTaskContext(PRRef): %v", err)
	}
	if task.TriggerType != "cli" || task.Changeset.PRNumber != 12 {
		t.Fatalf("unexpected task: trigger=%q pr=%d", task.TriggerType, task.Changeset.PRNumber)
	}
}
