package domain_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

func TestWorkspaceSpecCloneTokenNotSerialized(t *testing.T) {
	task := domain.TaskContext{
		RunID:        "run-123",
		WorkflowName: "pr-review",
		TriggerType:  "github_pr",
		DedupKey:     "github:owner/repo:pr:42",
		Workspace: domain.WorkspaceSpec{
			RepoURL:    "https://github.com/owner/repo.git",
			Ref:        "abc1234",
			BaseRef:    "main",
			CloneToken: "ghs_super_secret_ephemeral_token",
			Snapshot:   "mochi-default-polyglot",
		},
	}

	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	if strings.Contains(string(raw), "ghs_super_secret_ephemeral_token") || strings.Contains(string(raw), "clone_token") {
		t.Fatalf("CloneToken leaked into JSON output: %s", string(raw))
	}
}

func TestDurationRoundTrip(t *testing.T) {
	d := domain.Duration(3*time.Minute + 30*time.Second)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}
	if string(raw) != `"3m30s"` {
		t.Fatalf("unexpected JSON duration: %s", string(raw))
	}

	var decoded domain.Duration
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if decoded.Std() != d.Std() {
		t.Fatalf("got %v, want %v", decoded.Std(), d.Std())
	}
}
