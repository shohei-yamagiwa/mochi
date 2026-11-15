package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v93/github"
	"github.com/shohei-yamagiwa/mochi/internal/config"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	ghinput "github.com/shohei-yamagiwa/mochi/internal/input/github"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/orchestrator"
	"github.com/shohei-yamagiwa/mochi/internal/output"
	"github.com/shohei-yamagiwa/mochi/internal/server"
	"github.com/shohei-yamagiwa/mochi/internal/store"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
)

type stubLLM struct {
	name    string
	blockCh chan struct{}
}

func (s *stubLLM) Name() string { return s.name }

func (s *stubLLM) Generate(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if s.blockCh != nil {
		select {
		case <-s.blockCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if req.ResponseSchema != nil {
		rep := output.ReviewReport{
			Summary:  "Clean change.",
			Verdict:  output.VerdictApprove,
			Findings: []output.Finding{},
		}
		raw, _ := json.Marshal(rep)
		return &llm.ChatResponse{
			Message: llm.Message{
				Role:    llm.RoleAssistant,
				Content: string(raw),
			},
			Usage:        llm.TokenUsage{InputTokens: 10, OutputTokens: 10},
			FinishReason: "stop",
		}, nil
	}

	args := json.RawMessage(`{"findings":[],"summary":"No issues found."}`)
	return &llm.ChatResponse{
		Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "submit_output", Arguments: args},
			},
		},
		ToolCalls: []llm.ToolCall{
			{ID: "call-1", Name: "submit_output", Arguments: args},
		},
		Usage:        llm.TokenUsage{InputTokens: 10, OutputTokens: 10},
		FinishReason: "tool_calls",
	}, nil
}

func signHMAC(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func setupTestWorkflow(t *testing.T) (*domain.Workflow, string) {
	t.Helper()
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.md")
	synthPath := filepath.Join(dir, "synth.md")
	if err := os.WriteFile(specPath, []byte("Specialist prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(synthPath, []byte("Synthesizer prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	wf := &domain.Workflow{
		Version: "1",
		Name:    "pr-review",
		Input: domain.InputConfig{
			Adapter: "github_pr",
			Triggers: domain.TriggerConfig{
				Events:         []string{"opened", "synchronize"},
				CommentCommand: "/mochi review",
				IgnoreDrafts:   true,
			},
		},
		Sandbox: domain.SandboxConfig{
			Provider:     "daytona",
			ForkFromBase: false,
		},
		Agents: []domain.RoleConfig{
			{
				Role:       "correctness",
				PromptFile: "spec.md",
				LLM:        domain.ModelConfig{Provider: "openai", Model: "stub"},
				Sandbox:    domain.AgentSandboxConfig{Enabled: false},
				Tools:      []string{"submit_output"},
				Budgets:    domain.BudgetConfig{MaxTurns: 3},
			},
		},
		Synthesizer: domain.SynthesizerConfig{
			PromptFile: "synth.md",
			LLM:        domain.ModelConfig{Provider: "openai", Model: "stub"},
		},
		Output: domain.OutputConfig{
			Sinks: []domain.SinkConfig{{Type: "console"}},
		},
	}
	return wf, dir
}

func TestServer_WebhookTriggerInspectAndCancel(t *testing.T) {
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "mochi.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	var (
		mu          sync.Mutex
		mockGHCalls int
	)
	ghMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		mockGHCalls++
		mu.Unlock()
		switch r.URL.Path {
		case "/repos/shohei-yamagiwa/mochi/pulls/42":
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.v3.diff") {
				_, _ = w.Write([]byte("diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1,2 @@\n package main\n+var ok = true\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   42,
				"title":    "Add flag",
				"html_url": "https://github.com/shohei-yamagiwa/mochi/pull/42",
				"user":     map[string]any{"login": "shohei"},
				"base":     map[string]any{"ref": "main", "sha": "base1", "repo": map[string]any{"clone_url": "https://github.com/shohei-yamagiwa/mochi.git"}},
				"head":     map[string]any{"ref": "feat", "sha": "head2"},
			})
		case "/repos/shohei-yamagiwa/mochi/pulls/42/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "app.go", "status": "modified", "additions": 1, "patch": "@@ -1 +1,2 @@\n package main\n+var ok = true"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ghMock.Close()

	baseURL := ghMock.URL + "/"
	ghClient, err := github.NewClient(github.WithHTTPClient(ghMock.Client()), github.WithURLs(&baseURL, &baseURL))
	if err != nil {
		t.Fatal(err)
	}
	hookSecret := "gh-hook-secret"
	ghAdapter, err := ghinput.NewAdapter(
		ghinput.NewStaticClientFactory(ghClient, "tok"),
		ghinput.WithWebhookSecret(hookSecret),
	)
	if err != nil {
		t.Fatal(err)
	}

	wf, promptDir := setupTestWorkflow(t)
	runner := &orchestrator.Runner{
		Store:              st,
		DefaultLLMProvider: &stubLLM{name: "openai"},
		PromptEngine:       config.NewPromptEngine(promptDir),
	}
	sched := orchestrator.NewScheduler(runner, orchestrator.SchedulerConfig{MaxWorkers: 2})
	defer func() {
		_ = sched.Shutdown(context.Background())
	}()

	srv, err := server.New(server.Config{
		Workflows:       map[string]*domain.Workflow{wf.Name: wf},
		DefaultWorkflow: wf.Name,
		GitHubAdapter:   ghAdapter,
		Scheduler:       sched,
		Store:           st,
		TriggerSecret:   "trig-secret",
		APIToken:        "api-secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Health & readiness
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %v, err=%v", resp, err)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(ts.URL + "/readyz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %v, err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// 2. Invalid signature -> 401
	prBody := []byte(`{"action":"opened","number":42,"installation":{"id":1},"repository":{"name":"mochi","owner":{"login":"shohei-yamagiwa"}},"pull_request":{"number":42,"draft":false}}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewReader(prBody))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad signature, got %v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// 3. Ignored event (draft PR) -> 200
	draftBody := []byte(`{"action":"opened","number":43,"repository":{"name":"mochi","owner":{"login":"shohei-yamagiwa"}},"pull_request":{"number":43,"draft":true}}`)
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewReader(draftBody))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", signHMAC(hookSecret, draftBody))
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for ignored draft PR, got %v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// 4. Valid PR webhook -> 202 Accepted immediately + async hydration/execution
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewReader(prBody))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "deliv-42")
	req.Header.Set("X-Hub-Signature-256", signHMAC(hookSecret, prBody))
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 for valid webhook, got %v err=%v", resp, err)
	}
	var accepted struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if accepted.RunID == "" || accepted.Status != "accepted" {
		t.Fatalf("unexpected 202 payload: %+v", accepted)
	}

	if err := srv.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if _, err := sched.WaitForRun(context.Background(), accepted.RunID); err != nil {
		t.Fatalf("WaitForRun(%s): %v", accepted.RunID, err)
	}

	// 5. Inspect via API (verify auth required first)
	resp, err = http.Get(ts.URL + "/api/v1/runs")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without APIToken, got %v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+accepted.RunID, nil)
	req.Header.Set("Authorization", "Bearer api-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/runs/{id} = %v err=%v", resp, err)
	}
	var detail store.RunDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if detail.Run == nil || detail.Run.Status != domain.RunStatusCompleted || detail.Coverage == nil {
		t.Fatalf("unexpected RunDetail: %+v", detail)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/runs/"+accepted.RunID+"/coverage", nil)
	req.Header.Set("Authorization", "Bearer api-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET coverage = %v err=%v", resp, err)
	}
	var cov domain.CoverageReport
	if err := json.NewDecoder(resp.Body).Decode(&cov); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if cov.TotalChangedFiles != 1 {
		t.Fatalf("TotalChangedFiles = %d, want 1", cov.TotalChangedFiles)
	}

	// 6. Generic JSON trigger -> 202
	trigBody := []byte(`{"repo_url":"https://github.com/shohei-yamagiwa/mochi.git","ref":"main"}`)
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/triggers/pr-review", bytes.NewReader(trigBody))
	req.Header.Set("Authorization", "Bearer trig-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST trigger = %v err=%v", resp, err)
	}
	var trigResp struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&trigResp)
	_ = resp.Body.Close()
	if _, err := sched.WaitForRun(context.Background(), trigResp.RunID); err != nil {
		t.Fatalf("WaitForRun(trigger): %v", err)
	}
}

func TestServer_CancelActiveRun(t *testing.T) {
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "mochi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	wf, promptDir := setupTestWorkflow(t)
	blockCh := make(chan struct{})
	defer close(blockCh)

	runner := &orchestrator.Runner{
		Store:              st,
		DefaultLLMProvider: &stubLLM{name: "openai", blockCh: blockCh},
		PromptEngine:       config.NewPromptEngine(promptDir),
	}
	sched := orchestrator.NewScheduler(runner, orchestrator.SchedulerConfig{MaxWorkers: 1})
	defer func() {
		_ = sched.Shutdown(context.Background())
	}()

	srv, err := server.New(server.Config{
		Workflows:     map[string]*domain.Workflow{wf.Name: wf},
		Scheduler:     sched,
		Store:         st,
		TriggerSecret: "trig-secret",
		APIToken:      "api-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	trigBody := []byte(`{"repo_url":"https://github.com/shohei-yamagiwa/mochi.git","ref":"main"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/triggers/pr-review", bytes.NewReader(trigBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer trig-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST trigger = %v err=%v", resp, err)
	}
	var accepted struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&accepted)
	_ = resp.Body.Close()

	// Wait briefly until run enters scheduler, then cancel via API.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cancelReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/"+accepted.RunID+"/cancel", strings.NewReader(`{"reason":"user aborted"}`))
		cancelReq.Header.Set("Content-Type", "application/json")
		cancelReq.Header.Set("Authorization", "Bearer api-secret")
		cancelResp, err := http.DefaultClient.Do(cancelReq)
		if err != nil {
			t.Fatal(err)
		}
		_ = cancelResp.Body.Close()
		if cancelResp.StatusCode == http.StatusAccepted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, waitErr := sched.WaitForRun(context.Background(), accepted.RunID)
	if waitErr == nil {
		t.Fatalf("expected cancelled run to return error from WaitForRun")
	}
}

func TestServer_FailClosedWhenSecretsMissing(t *testing.T) {
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "mochi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	wf, promptDir := setupTestWorkflow(t)
	runner := &orchestrator.Runner{
		Store:              st,
		DefaultLLMProvider: &stubLLM{name: "openai"},
		PromptEngine:       config.NewPromptEngine(promptDir),
	}
	sched := orchestrator.NewScheduler(runner, orchestrator.SchedulerConfig{MaxWorkers: 1})
	defer func() {
		_ = sched.Shutdown(context.Background())
	}()

	ghClient, err := github.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	clients := ghinput.NewStaticClientFactory(ghClient, "tok")
	ghAdapterFailClosed, err := ghinput.NewAdapter(clients)
	if err != nil {
		t.Fatal(err)
	}

	srv, err := server.New(server.Config{
		Workflows:       map[string]*domain.Workflow{wf.Name: wf},
		DefaultWorkflow: wf.Name,
		GitHubAdapter:   ghAdapterFailClosed,
		Scheduler:       sched,
		Store:           st,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Without secrets and without AllowInsecureNoAuth, all protected endpoints must return 401.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
		header map[string]string
	}{
		{
			name:   "github webhook",
			method: http.MethodPost,
			path:   "/webhooks/github",
			body:   `{"action":"opened","number":43,"repository":{"name":"mochi","owner":{"login":"shohei-yamagiwa"}},"pull_request":{"number":43,"draft":true}}`,
			header: map[string]string{"X-GitHub-Event": "pull_request"},
		},
		{
			name:   "trigger endpoint",
			method: http.MethodPost,
			path:   "/api/v1/triggers/pr-review",
			body:   `{"repo_url":"https://github.com/shohei-yamagiwa/mochi.git","ref":"main"}`,
		},
		{
			name:   "list runs",
			method: http.MethodGet,
			path:   "/api/v1/runs",
		},
		{
			name:   "cancel run",
			method: http.MethodPost,
			path:   "/api/v1/runs/run-missing/cancel",
			body:   `{}`,
		},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(tc.body))
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s request failed: %v", tc.name, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", tc.name, resp.StatusCode)
		}
	}

	// Explicit opt-in allows unauthenticated requests for local development.
	ghAdapterInsecure, err := ghinput.NewAdapter(clients, ghinput.WithAllowInsecureNoWebhookSecret(true))
	if err != nil {
		t.Fatal(err)
	}
	insecureSrv, err := server.New(server.Config{
		Workflows:           map[string]*domain.Workflow{wf.Name: wf},
		DefaultWorkflow:     wf.Name,
		GitHubAdapter:       ghAdapterInsecure,
		Scheduler:           sched,
		Store:               st,
		AllowInsecureNoAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	insecureTS := httptest.NewServer(insecureSrv.Handler())
	defer insecureTS.Close()

	resp, err := http.Get(insecureTS.URL + "/api/v1/runs")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("insecure GET /api/v1/runs status = %d, want 200", resp.StatusCode)
	}

	trigReq, _ := http.NewRequest(http.MethodPost, insecureTS.URL+"/api/v1/triggers/pr-review",
		strings.NewReader(`{"repo_url":"https://github.com/shohei-yamagiwa/mochi.git","ref":"main"}`))
	trigResp, err := http.DefaultClient.Do(trigReq)
	if err != nil {
		t.Fatal(err)
	}
	var trigAccepted struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(trigResp.Body).Decode(&trigAccepted)
	_ = trigResp.Body.Close()
	if trigResp.StatusCode != http.StatusAccepted {
		t.Fatalf("insecure POST trigger status = %d, want 202", trigResp.StatusCode)
	}
	if _, err := sched.WaitForRun(context.Background(), trigAccepted.RunID); err != nil {
		t.Fatalf("WaitForRun(insecure trigger): %v", err)
	}

	whReq, _ := http.NewRequest(http.MethodPost, insecureTS.URL+"/webhooks/github",
		strings.NewReader(`{"action":"opened","number":43,"repository":{"name":"mochi","owner":{"login":"shohei-yamagiwa"}},"pull_request":{"number":43,"draft":true}}`))
	whReq.Header.Set("X-GitHub-Event", "pull_request")
	whResp, err := http.DefaultClient.Do(whReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = whResp.Body.Close()
	if whResp.StatusCode != http.StatusOK {
		t.Fatalf("insecure webhook draft PR status = %d, want 200", whResp.StatusCode)
	}
}
