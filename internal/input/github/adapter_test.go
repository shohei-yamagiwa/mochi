package github_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-github/v93/github"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	ghinput "github.com/shohei-yamagiwa/mochi/internal/input/github"
)

func signPayload(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignatureAndParseWebhookEvents(t *testing.T) {
	secret := "super-secret-webhook-key"
	client, err := github.NewClient()
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}
	adapter, err := ghinput.NewAdapter(
		ghinput.NewStaticClientFactory(client, "clone-tok"),
		ghinput.WithWebhookSecret(secret),
	)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	prPayload := []byte(`{
		"action": "opened",
		"number": 42,
		"installation": {"id": 9001},
		"repository": {"name": "mochi", "owner": {"login": "shohei-yamagiwa"}},
		"pull_request": {"number": 42, "draft": false}
	}`)

	if err := adapter.VerifySignature(prPayload, signPayload(secret, prPayload)); err != nil {
		t.Fatalf("VerifySignature valid failed: %v", err)
	}
	if err := adapter.VerifySignature(prPayload, "sha256=deadbeef"); !errors.Is(err, ghinput.ErrInvalidSignature) {
		t.Fatalf("VerifySignature invalid = %v, want ErrInvalidSignature", err)
	}

	target, err := adapter.ParseWebhookEvent("pull_request", prPayload, nil)
	if err != nil {
		t.Fatalf("ParseWebhookEvent(pull_request) failed: %v", err)
	}
	if target.Owner != "shohei-yamagiwa" || target.Repo != "mochi" || target.PRNumber != 42 || target.InstallationID != 9001 {
		t.Fatalf("unexpected target: %+v", target)
	}

	// Draft PR should be ignored when IgnoreDrafts=true.
	draftPayload := []byte(`{
		"action": "opened",
		"number": 43,
		"repository": {"name": "mochi", "owner": {"login": "shohei-yamagiwa"}},
		"pull_request": {"number": 43, "draft": true}
	}`)
	if _, err := adapter.ParseWebhookEvent("pull_request", draftPayload, nil); !errors.Is(err, ghinput.ErrIgnoredEvent) {
		t.Fatalf("draft PR error = %v, want ErrIgnoredEvent", err)
	}

	// /mochi review issue_comment on an open PR by an authorized commenter should trigger.
	commentPayload := []byte(`{
		"action": "created",
		"installation": {"id": 9001},
		"repository": {"name": "mochi", "owner": {"login": "shohei-yamagiwa"}},
		"sender": {"login": "alice", "type": "User"},
		"issue": {"number": 42, "state": "open", "pull_request": {"url": "https://api.github.com/repos/shohei-yamagiwa/mochi/pulls/42"}},
		"comment": {"body": "Please check this\n/mochi review", "author_association": "OWNER"}
	}`)
	commentTarget, err := adapter.ParseWebhookEvent("issue_comment", commentPayload, nil)
	if err != nil {
		t.Fatalf("ParseWebhookEvent(issue_comment) failed: %v", err)
	}
	if commentTarget.PRNumber != 42 || commentTarget.TriggerEvent != "issue_comment.command" {
		t.Fatalf("unexpected commentTarget: %+v", commentTarget)
	}

	// Unauthorized commenter associations should be ignored.
	for _, assoc := range []string{"NONE", "CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", ""} {
		var unauthPayload string
		if assoc == "" {
			unauthPayload = strings.ReplaceAll(string(commentPayload), `, "author_association": "OWNER"`, ``)
		} else {
			unauthPayload = strings.ReplaceAll(string(commentPayload), `"OWNER"`, `"`+assoc+`"`)
		}
		if _, err := adapter.ParseWebhookEvent("issue_comment", []byte(unauthPayload), nil); !errors.Is(err, ghinput.ErrIgnoredEvent) {
			t.Fatalf("association %q error = %v, want ErrIgnoredEvent", assoc, err)
		}
	}

	// Custom AllowedCommentAssociations override allows MEMBER or CONTRIBUTOR when configured.
	customTriggers := &domain.TriggerConfig{
		CommentCommand:             "/mochi review",
		AllowedCommentAssociations: []string{"CONTRIBUTOR"},
		IgnoreDrafts:               true,
	}
	contribPayload := strings.ReplaceAll(string(commentPayload), `"OWNER"`, `"CONTRIBUTOR"`)
	if _, err := adapter.ParseWebhookEvent("issue_comment", []byte(contribPayload), customTriggers); err != nil {
		t.Fatalf("custom AllowedCommentAssociations failed: %v", err)
	}

	// Comment on a closed PR should be ignored.
	closedCommentPayload := strings.ReplaceAll(string(commentPayload), `"state": "open"`, `"state": "closed"`)
	if _, err := adapter.ParseWebhookEvent("issue_comment", []byte(closedCommentPayload), nil); !errors.Is(err, ghinput.ErrIgnoredEvent) {
		t.Fatalf("closed PR comment error = %v, want ErrIgnoredEvent", err)
	}

	// Bot comment should be ignored.
	botCommentPayload := strings.ReplaceAll(string(commentPayload), `"type": "User"`, `"type": "Bot"`)
	if _, err := adapter.ParseWebhookEvent("issue_comment", []byte(botCommentPayload), nil); !errors.Is(err, ghinput.ErrIgnoredEvent) {
		t.Fatalf("bot comment error = %v, want ErrIgnoredEvent", err)
	}

	// Unrelated comment should be ignored.
	otherComment := strings.ReplaceAll(string(commentPayload), `/mochi review`, `LGTM!`)
	if _, err := adapter.ParseWebhookEvent("issue_comment", []byte(otherComment), nil); !errors.Is(err, ghinput.ErrIgnoredEvent) {
		t.Fatalf("non-command comment error = %v, want ErrIgnoredEvent", err)
	}
}

func TestBuildTaskContextAndBaseBranchRepoOverrides(t *testing.T) {
	unifiedDiff := `diff --git a/internal/auth/service.go b/internal/auth/service.go
index 1111111..2222222 100644
--- a/internal/auth/service.go
+++ b/internal/auth/service.go
@@ -10,2 +10,3 @@ func Verify(tok string) error {
 	if tok == "" {
+		return ErrEmpty
 	}
`
	var requestedRefs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/42":
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.v3.diff") {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(unifiedDiff))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   42,
				"title":    "Fix empty token check",
				"body":     "Rejects empty bearer tokens",
				"html_url": "https://github.com/shohei-yamagiwa/mochi/pull/42",
				"draft":    false,
				"user":     map[string]any{"login": "shohei"},
				"base": map[string]any{
					"ref": "main",
					"sha": "base1111111",
					"repo": map[string]any{
						"full_name": "shohei-yamagiwa/mochi",
						"clone_url": "https://github.com/shohei-yamagiwa/mochi.git",
					},
				},
				"head": map[string]any{
					"ref": "fix/auth",
					"sha": "head2222222",
					"repo": map[string]any{
						"full_name": "fork-user/mochi",
					},
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/42/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"filename":  "internal/auth/service.go",
					"status":    "modified",
					"additions": 1,
					"deletions": 0,
					"patch":     "@@ -10,2 +10,3 @@ func Verify(tok string) error {\n \tif tok == \"\" {\n+\t\treturn ErrEmpty\n \t}",
				},
			})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/shohei-yamagiwa/mochi/contents/"):
			requestedRefs = append(requestedRefs, r.URL.Query().Get("ref"))
			path := strings.TrimPrefix(r.URL.Path, "/repos/shohei-yamagiwa/mochi/contents/")
			var content string
			switch path {
			case ".mochi/config.yaml":
				content = "agents:\n  - role: correctness\n    prompt_file: \".mochi/prompts/custom.md\"\n"
			case ".mochi/prompts/custom.md":
				content = "Custom base-branch prompt for {{ .Task.Changeset.RepoName }}"
			default:
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(content)),
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
		t.Fatalf("github.NewClient: %v", err)
	}
	clients := ghinput.NewStaticClientFactory(ghClient, "ghs_ephemeral_read_only")

	secret := "hook-secret"
	adapter, err := ghinput.NewAdapter(clients, ghinput.WithWebhookSecret(secret))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	payload := []byte(`{
		"action": "synchronize",
		"number": 42,
		"installation": {"id": 77},
		"repository": {"name": "mochi", "owner": {"login": "shohei-yamagiwa"}},
		"pull_request": {"number": 42, "draft": false}
	}`)

	task, err := adapter.BuildTaskContext(context.Background(), &ghinput.WebhookEvent{
		EventType:    "pull_request",
		Signature256: signPayload(secret, payload),
		Payload:      payload,
	})
	if err != nil {
		t.Fatalf("BuildTaskContext: %v", err)
	}

	if task.DedupKey != "github:shohei-yamagiwa/mochi:pr:42" {
		t.Fatalf("DedupKey = %q, want github:shohei-yamagiwa/mochi:pr:42", task.DedupKey)
	}
	if task.Workspace.Ref != "head2222222" || task.Workspace.BaseRef != "base1111111" ||
		task.Workspace.BaseBranch != "main" || task.Workspace.HeadBranch != "fix/auth" ||
		!slices.Equal(task.Workspace.FetchRefs, []string{"refs/pull/42/head"}) {
		t.Fatalf("unexpected Workspace: %+v", task.Workspace)
	}
	if task.Workspace.CloneToken != "ghs_ephemeral_read_only" {
		t.Fatalf("CloneToken = %q, want ghs_ephemeral_read_only", task.Workspace.CloneToken)
	}
	if task.Changeset == nil || len(task.Changeset.ChangedFiles) != 1 {
		t.Fatalf("unexpected Changeset: %+v", task.Changeset)
	}
	wantHunks := []domain.LineRange{{StartLine: 10, EndLine: 12}}
	if !slices.Equal(task.Changeset.ChangedFiles[0].ChangedHunks, wantHunks) {
		t.Fatalf("ChangedHunks = %+v, want %+v", task.Changeset.ChangedFiles[0].ChangedHunks, wantHunks)
	}

	// Verify RepoOverrideLoader fetches .mochi/ exclusively from BaseSHA (not HeadSHA).
	loader := ghinput.NewRepoOverrideLoader(clients, true)
	bundle, err := loader(context.Background(), task, nil, ".mochi/config.yaml")
	if err != nil {
		t.Fatalf("RepoOverrideLoader: %v", err)
	}
	if bundle == nil || !strings.Contains(string(bundle.ConfigYAML), "custom.md") {
		t.Fatalf("unexpected RepoOverrideBundle: %+v", bundle)
	}
	defer os.RemoveAll(bundle.PromptDir)

	for _, ref := range requestedRefs {
		if ref != "base1111111" {
			t.Fatalf("expected repo override fetch from base1111111, got ref %q", ref)
		}
	}
	promptBytes, err := os.ReadFile(filepath.Join(bundle.PromptDir, ".mochi", "prompts", "custom.md"))
	if err != nil {
		t.Fatalf("read cached base prompt: %v", err)
	}
	if !strings.Contains(string(promptBytes), "Custom base-branch prompt") {
		t.Fatalf("unexpected prompt content: %q", string(promptBytes))
	}
}

func TestVerifySignature_FailClosedByDefault(t *testing.T) {
	client, err := github.NewClient()
	if err != nil {
		t.Fatalf("github.NewClient: %v", err)
	}
	clients := ghinput.NewStaticClientFactory(client, "tok")

	adapter, err := ghinput.NewAdapter(clients)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	if err := adapter.VerifySignature([]byte(`{"ok":true}`), ""); !errors.Is(err, ghinput.ErrInvalidSignature) {
		t.Fatalf("VerifySignature(empty secret, default) = %v, want ErrInvalidSignature", err)
	}
	if err := adapter.VerifySignature([]byte(`{"ok":true}`), "sha256=deadbeef"); !errors.Is(err, ghinput.ErrInvalidSignature) {
		t.Fatalf("VerifySignature(empty secret, forged sig) = %v, want ErrInvalidSignature", err)
	}

	insecureAdapter, err := ghinput.NewAdapter(clients, ghinput.WithAllowInsecureNoWebhookSecret(true))
	if err != nil {
		t.Fatalf("NewAdapter insecure: %v", err)
	}
	if err := insecureAdapter.VerifySignature([]byte(`{"ok":true}`), ""); err != nil {
		t.Fatalf("VerifySignature(insecure opt-in) failed: %v", err)
	}
}

func TestBuildTaskContext_IgnoresClosedMergedAndDraftPR(t *testing.T) {
	tests := []struct {
		name string
		pr   map[string]any
	}{
		{
			name: "closed PR",
			pr: map[string]any{
				"number": 10,
				"state":  "closed",
				"draft":  false,
				"merged": false,
			},
		},
		{
			name: "merged PR",
			pr: map[string]any{
				"number": 11,
				"state":  "open",
				"draft":  false,
				"merged": true,
			},
		},
		{
			name: "draft PR via issue_comment with nil triggers",
			pr: map[string]any{
				"number": 12,
				"state":  "open",
				"draft":  true,
				"merged": false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/shohei-yamagiwa/mochi/pulls/") {
					_ = json.NewEncoder(w).Encode(tc.pr)
					return
				}
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}))
			defer srv.Close()

			baseURL := srv.URL + "/"
			ghClient, err := github.NewClient(github.WithHTTPClient(srv.Client()), github.WithURLs(&baseURL, &baseURL))
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := ghinput.NewAdapter(ghinput.NewStaticClientFactory(ghClient, "tok"))
			if err != nil {
				t.Fatal(err)
			}

			num := tc.pr["number"].(int)
			_, err = adapter.BuildTaskContext(context.Background(), &ghinput.PRTarget{
				Owner:        "shohei-yamagiwa",
				Repo:         "mochi",
				PRNumber:     num,
				TriggerEvent: "issue_comment.command",
			})
			if !errors.Is(err, ghinput.ErrIgnoredEvent) {
				t.Fatalf("%s: got err=%v, want ErrIgnoredEvent", tc.name, err)
			}
		})
	}
}

func TestBuildTaskContext_AllowsNonWebhookDraftAndClosedMergedPR(t *testing.T) {
	tests := []struct {
		name  string
		state string
		draft bool
		merge bool
	}{
		{name: "draft open PR", state: "open", draft: true, merge: false},
		{name: "closed merged PR", state: "closed", draft: false, merge: true},
		{name: "closed unmerged PR", state: "closed", draft: false, merge: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/shohei-yamagiwa/mochi/pulls/12":
					if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.v3.diff") {
						_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n package a\n+var x = 1\n"))
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number":   12,
						"title":    "Non-webhook PR",
						"state":    tc.state,
						"draft":    tc.draft,
						"merged":   tc.merge,
						"html_url": "https://github.com/shohei-yamagiwa/mochi/pull/12",
						"user":     map[string]any{"login": "shohei"},
						"base":     map[string]any{"ref": "main", "sha": "base123", "repo": map[string]any{"clone_url": "https://github.com/shohei-yamagiwa/mochi.git"}},
						"head":     map[string]any{"ref": "feat", "sha": "head456"},
					})
				case "/repos/shohei-yamagiwa/mochi/pulls/12/files":
					_ = json.NewEncoder(w).Encode([]map[string]any{
						{"filename": "a.go", "status": "modified", "additions": 1, "patch": "@@ -1 +1,2 @@\n package a\n+var x = 1"},
					})
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
			adapter, err := ghinput.NewAdapter(ghinput.NewStaticClientFactory(ghClient, "tok"))
			if err != nil {
				t.Fatal(err)
			}

			task, err := adapter.BuildTaskContext(context.Background(), &ghinput.PRTarget{
				Owner:        "shohei-yamagiwa",
				Repo:         "mochi",
				PRNumber:     12,
				TriggerEvent: "cli.pr",
			})
			if err != nil {
				t.Fatalf("%s: BuildTaskContext unexpected error: %v", tc.name, err)
			}
			if task.Changeset == nil || task.Changeset.PRNumber != 12 {
				t.Fatalf("%s: unexpected Changeset: %+v", tc.name, task.Changeset)
			}
		})
	}
}

func TestMatchBranchPatternAndMatchBranches(t *testing.T) {
	if !ghinput.MatchBranches(nil, "anything/goes") {
		t.Fatal("empty patterns should match all branches")
	}
	if ghinput.MatchBranches([]string{"main"}, "") {
		t.Fatal("non-empty patterns must not match empty branch")
	}

	cases := []struct {
		pattern string
		branch  string
		want    bool
	}{
		{"main", "main", true},
		{"main", "main2", false},
		{"feature/*", "feature/login", true},
		{"feature/*", "feature/team/login", false},
		{"feature/**", "feature/login", true},
		{"feature/**", "feature/team/login", true},
		{"release/v?.0", "release/v1.0", true},
		{"release/v?.0", "release/v10.0", false},
		{"**/main", "main", true},
		{"**/main", "org/main", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
	}
	for _, tc := range cases {
		if got := ghinput.MatchBranchPattern(tc.pattern, tc.branch); got != tc.want {
			t.Errorf("MatchBranchPattern(%q, %q) = %v, want %v", tc.pattern, tc.branch, got, tc.want)
		}
	}
}

func TestBuildTaskContext_BranchTriggerFiltering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/shohei-yamagiwa/mochi/pulls/55":
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.v3.diff") {
				_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n package a\n+var x = 1\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   55,
				"title":    "Branch filter PR",
				"state":    "open",
				"draft":    false,
				"merged":   false,
				"html_url": "https://github.com/shohei-yamagiwa/mochi/pull/55",
				"user":     map[string]any{"login": "shohei"},
				"base":     map[string]any{"ref": "develop", "sha": "base55", "repo": map[string]any{"clone_url": "https://github.com/shohei-yamagiwa/mochi.git"}},
				"head":     map[string]any{"ref": "feature/ui", "sha": "head55"},
			})
		case "/repos/shohei-yamagiwa/mochi/pulls/55/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "a.go", "status": "modified", "additions": 1, "patch": "@@ -1 +1,2 @@\n package a\n+var x = 1"},
			})
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
	adapter, err := ghinput.NewAdapter(ghinput.NewStaticClientFactory(ghClient, "tok"), ghinput.WithAllowInsecureNoWebhookSecret(true))
	if err != nil {
		t.Fatal(err)
	}

	// Pull request targeting develop should be ignored when trigger requires base_branches: ["main"].
	prPayload := []byte(`{
		"action": "opened",
		"number": 55,
		"repository": {"name": "mochi", "owner": {"login": "shohei-yamagiwa"}},
		"pull_request": {
			"number": 55,
			"draft": false,
			"base": {"ref": "develop"},
			"head": {"ref": "feature/ui"}
		}
	}`)
	if _, err := adapter.ParseWebhookEvent("pull_request", prPayload, &domain.TriggerConfig{
		BaseBranches: []string{"main"},
		IgnoreDrafts: true,
	}); !errors.Is(err, ghinput.ErrIgnoredEvent) {
		t.Fatalf("expected ErrIgnoredEvent for base mismatch, got %v", err)
	}

	// Issue comment command on PR targeting develop should be ignored after PullRequests.Get if base_branches=["main"].
	commentPayload := []byte(`{
		"action": "created",
		"repository": {"name": "mochi", "owner": {"login": "shohei-yamagiwa"}},
		"sender": {"login": "shohei", "type": "User"},
		"issue": {"number": 55, "state": "open", "pull_request": {"url": "https://api.github.com/repos/shohei-yamagiwa/mochi/pulls/55"}},
		"comment": {"body": "/mochi review", "author_association": "OWNER"}
	}`)
	if _, err := adapter.BuildTaskContext(context.Background(), &ghinput.WebhookEvent{
		EventType: "issue_comment",
		Payload:   commentPayload,
		Triggers: &domain.TriggerConfig{
			BaseBranches: []string{"main"},
			IgnoreDrafts: true,
		},
	}); !errors.Is(err, ghinput.ErrIgnoredEvent) {
		t.Fatalf("expected ErrIgnoredEvent for issue_comment base mismatch, got %v", err)
	}

	// Matching base_branches + head_branches succeeds.
	task, err := adapter.BuildTaskContext(context.Background(), &ghinput.WebhookEvent{
		EventType: "issue_comment",
		Payload:   commentPayload,
		Triggers: &domain.TriggerConfig{
			BaseBranches: []string{"develop"},
			HeadBranches: []string{"feature/*"},
			IgnoreDrafts: true,
		},
	})
	if err != nil {
		t.Fatalf("expected matching branch trigger to succeed, got %v", err)
	}
	if task.Changeset.BaseBranch != "develop" || task.Changeset.HeadBranch != "feature/ui" {
		t.Fatalf("unexpected branches: %+v", task.Changeset)
	}
}

func TestBuildTaskContextsForRepoWorkflows(t *testing.T) {
	mainWF := `version: "1"
name: pr-review
input:
  adapter: github_pr
  triggers:
    events: ["opened", "synchronize"]
    base_branches: ["main"]
    comment_command: "/mochi review"
    ignore_drafts: true
sandbox:
  provider: daytona
agents:
  - role: correctness
    prompt_file: "configs/prompts/correctness.md"
    llm:
      provider: gemini
      model: gemini-4-hydrogen
    tools: ["read_file", "submit_output"]
synthesizer:
  prompt_file: "configs/prompts/synthesizer.md"
  llm:
    provider: gemini
    model: gemini-4-hydrogen
output:
  sinks:
    - type: github_pr_review
`

	developSmallWF := `version: "1"
name: pr-review-small
input:
  adapter: github_pr
  triggers:
    events: ["opened", "synchronize"]
    base_branches: ["develop"]
    head_branches: ["feature/**"]
    comment_command: "/mochi review-small"
    ignore_drafts: true
sandbox:
  provider: daytona
agents:
  - role: correctness
    prompt_file: ".mochi/prompts/small.md"
    llm:
      provider: gemini
      model: gemini-4-hydrogen
    tools: ["read_file", "submit_output"]
synthesizer:
  prompt_file: "configs/prompts/synthesizer.md"
  llm:
    provider: gemini
    model: gemini-4-hydrogen
output:
  sinks:
    - type: github_pr_review
`

	var baseBranch string
	var headBranch string
	var requestedRefs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/77":
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.v3.diff") {
				_, _ = w.Write([]byte("diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n package a\n+var x = 1\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   77,
				"title":    "Multi-workflow PR",
				"state":    "open",
				"draft":    false,
				"merged":   false,
				"html_url": "https://github.com/shohei-yamagiwa/mochi/pull/77",
				"user":     map[string]any{"login": "shohei"},
				"base":     map[string]any{"ref": baseBranch, "sha": "base777", "repo": map[string]any{"clone_url": "https://github.com/shohei-yamagiwa/mochi.git"}},
				"head":     map[string]any{"ref": headBranch, "sha": "head777"},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/77/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "a.go", "status": "modified", "additions": 1, "patch": "@@ -1 +1,2 @@\n package a\n+var x = 1"},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/contents/.mochi/workflows":
			requestedRefs = append(requestedRefs, r.URL.Query().Get("ref"))
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "pr-review.yaml", "path": ".mochi/workflows/pr-review.yaml", "type": "file"},
				{"name": "pr-review-small.yaml", "path": ".mochi/workflows/pr-review-small.yaml", "type": "file"},
				{"name": "README.md", "path": ".mochi/workflows/README.md", "type": "file"},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/contents/.mochi/workflows/pr-review.yaml":
			requestedRefs = append(requestedRefs, r.URL.Query().Get("ref"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(mainWF)),
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/contents/.mochi/workflows/pr-review-small.yaml":
			requestedRefs = append(requestedRefs, r.URL.Query().Get("ref"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(developSmallWF)),
			})

		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/contents/.mochi/prompts/small.md":
			requestedRefs = append(requestedRefs, r.URL.Query().Get("ref"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte("SMALL_BASE_PROMPT")),
			})

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
	adapter, err := ghinput.NewAdapter(ghinput.NewStaticClientFactory(ghClient, "tok"))
	if err != nil {
		t.Fatal(err)
	}

	// 1. feature/team/add -> develop matches pr-review-small only, via custom comment command /mochi review-small.
	baseBranch = "develop"
	headBranch = "feature/team/add"
	requestedRefs = nil
	cand, err := adapter.ParseWebhookCandidate("issue_comment", []byte(`{
		"action": "created",
		"installation": {"id": 99},
		"repository": {"name": "mochi", "owner": {"login": "shohei-yamagiwa"}},
		"sender": {"login": "shohei", "type": "User"},
		"issue": {"number": 77, "state": "open", "pull_request": {"url": "https://api.github.com/repos/shohei-yamagiwa/mochi/pulls/77"}},
		"comment": {"body": "/mochi review-small", "author_association": "OWNER"}
	}`))
	if err != nil {
		t.Fatalf("ParseWebhookCandidate: %v", err)
	}
	tasks, err := adapter.BuildTaskContextsForRepoWorkflows(context.Background(), cand)
	if err != nil {
		t.Fatalf("BuildTaskContextsForRepoWorkflows: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Workflow.Name != "pr-review-small" {
		t.Fatalf("got %d tasks (%v), want [pr-review-small]", len(tasks), tasks)
	}
	defer os.RemoveAll(tasks[0].Task.PromptDir)

	if tasks[0].Task.DedupKey != "github:shohei-yamagiwa/mochi:pr:77:workflow:pr-review-small" {
		t.Fatalf("DedupKey = %q", tasks[0].Task.DedupKey)
	}
	if !tasks[0].Workflow.Agents[0].PromptFromRepo {
		t.Fatal("expected .mochi/prompts/small.md to be marked PromptFromRepo=true")
	}
	promptData, err := os.ReadFile(filepath.Join(tasks[0].Task.PromptDir, ".mochi", "prompts", "small.md"))
	if err != nil || string(promptData) != "SMALL_BASE_PROMPT" {
		t.Fatalf("unexpected materialized prompt: err=%v data=%q", err, string(promptData))
	}
	for _, ref := range requestedRefs {
		if ref != "base777" {
			t.Fatalf("expected all workflow/prompt fetches from base777, got %q", ref)
		}
	}

	// 2. develop -> main pull_request opened matches pr-review only (and requires no PromptDir since it uses built-in prompts).
	baseBranch = "main"
	headBranch = "develop"
	prCand, err := adapter.ParseWebhookCandidate("pull_request", []byte(`{
		"action": "opened",
		"number": 77,
		"installation": {"id": 99},
		"repository": {"name": "mochi", "owner": {"login": "shohei-yamagiwa"}},
		"pull_request": {"number": 77, "draft": false}
	}`))
	if err != nil {
		t.Fatalf("ParseWebhookCandidate(pr): %v", err)
	}
	mainTasks, err := adapter.BuildTaskContextsForRepoWorkflows(context.Background(), prCand)
	if err != nil {
		t.Fatalf("BuildTaskContextsForRepoWorkflows(main): %v", err)
	}
	if len(mainTasks) != 1 || mainTasks[0].Workflow.Name != "pr-review" {
		t.Fatalf("got %d tasks, want [pr-review]", len(mainTasks))
	}
	if mainTasks[0].Task.PromptDir != "" {
		t.Fatalf("expected empty PromptDir for workflow using built-in prompts, got %q", mainTasks[0].Task.PromptDir)
	}
}
