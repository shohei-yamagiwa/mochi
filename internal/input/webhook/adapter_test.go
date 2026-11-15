package webhook_test

import (
	"context"
	"testing"

	"github.com/shohei-yamagiwa/mochi/internal/input/webhook"
)

func TestWebhookAdapter_BuildTaskContext(t *testing.T) {
	adapter := webhook.NewAdapter("pr-review")
	if adapter.Name() != "webhook" {
		t.Fatalf("Name() = %q, want webhook", adapter.Name())
	}

	payload := []byte(`{
		"repo_url": "https://github.com/shohei-yamagiwa/mochi.git",
		"ref": "head123",
		"base_ref": "main",
		"clone_token": "secret-tok",
		"changeset": {
			"repo_owner": "shohei-yamagiwa",
			"repo_name": "mochi",
			"pr_number": 9,
			"base_sha": "base111",
			"head_sha": "head123",
			"unified_diff": "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,2 +1,3 @@\n package main\n+var x = 1\n"
		},
		"variables": {"env": "ci"}
	}`)

	task, err := adapter.BuildTaskContext(context.Background(), payload)
	if err != nil {
		t.Fatalf("BuildTaskContext: %v", err)
	}
	if task.WorkflowName != "pr-review" {
		t.Fatalf("WorkflowName = %q, want pr-review", task.WorkflowName)
	}
	if task.DedupKey != "webhook:shohei-yamagiwa/mochi:pr:9" {
		t.Fatalf("DedupKey = %q, want webhook:shohei-yamagiwa/mochi:pr:9", task.DedupKey)
	}
	if task.Workspace.CloneToken != "secret-tok" {
		t.Fatalf("CloneToken = %q, want secret-tok", task.Workspace.CloneToken)
	}
	if task.Changeset == nil || len(task.Changeset.ChangedFiles) != 1 {
		t.Fatalf("expected 1 parsed ChangedFile, got %+v", task.Changeset)
	}
	if task.Variables["env"] != "ci" {
		t.Fatalf("Variables[env] = %v, want ci", task.Variables["env"])
	}

	if _, err := adapter.BuildTaskContext(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("expected error for empty webhook payload")
	}
}
