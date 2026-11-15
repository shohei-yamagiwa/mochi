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
