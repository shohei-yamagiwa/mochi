package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
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
	"github.com/shohei-yamagiwa/mochi/internal/credentials"
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

type recordedLLM struct {
	mu       sync.Mutex
	contents []string
}

func (r *recordedLLM) Name() string { return "openai" }

func (r *recordedLLM) Generate(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if req.ResponseSchema != nil {
		raw, _ := json.Marshal(output.ReviewReport{Summary: "Clean.", Verdict: output.VerdictApprove})
		return &llm.ChatResponse{
			Message:      llm.Message{Role: llm.RoleAssistant, Content: string(raw)},
			FinishReason: "stop",
		}, nil
	}
	r.mu.Lock()
	for _, m := range req.Messages {
		r.contents = append(r.contents, m.Content)
	}
	r.mu.Unlock()
	args := json.RawMessage(`{"findings":[],"summary":"Clean."}`)
	return &llm.ChatResponse{
		Message:   llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "submit_output", Arguments: args}}},
		ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "submit_output", Arguments: args}},
	}, nil
}

func TestServer_OutOfOrderWebhookHydration(t *testing.T) {
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "mochi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	firstHydrating := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseFirst) })
	var reqMu sync.Mutex
	prCalls := 0

	ghMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/shohei-yamagiwa/mochi/pulls/42":
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.v3.diff") {
				_, _ = w.Write([]byte("diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1,2 @@\n package main\n+var ok = true\n"))
				return
			}
			reqMu.Lock()
			prCalls++
			callNum := prCalls
			reqMu.Unlock()

			headSHA := "head_newer_2"
			title := "Newer push"
			if callNum == 1 {
				headSHA = "head_older_1"
				title = "Older push"
				firstOnce.Do(func() { close(firstHydrating) })
				<-releaseFirst
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   42,
				"state":    "open",
				"title":    title,
				"html_url": "https://github.com/shohei-yamagiwa/mochi/pull/42",
				"user":     map[string]any{"login": "shohei"},
				"base":     map[string]any{"ref": "main", "sha": "base1", "repo": map[string]any{"clone_url": "https://github.com/shohei-yamagiwa/mochi.git"}},
				"head":     map[string]any{"ref": "feat", "sha": headSHA},
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
	hookSecret := "hook-secret"
	ghAdapter, err := ghinput.NewAdapter(ghinput.NewStaticClientFactory(ghClient, "tok"), ghinput.WithWebhookSecret(hookSecret))
	if err != nil {
		t.Fatal(err)
	}

	wf, promptDir := setupTestWorkflow(t)
	if err := os.WriteFile(filepath.Join(promptDir, "spec.md"), []byte("Head={{ .Task.Changeset.HeadSHA }}"), 0o644); err != nil {
		t.Fatal(err)
	}
	recLLM := &recordedLLM{}
	runner := &orchestrator.Runner{
		Store:              st,
		DefaultLLMProvider: recLLM,
		PromptEngine:       config.NewPromptEngine(promptDir),
	}
	sched := orchestrator.NewScheduler(runner, orchestrator.SchedulerConfig{MaxWorkers: 2})
	defer func() { _ = sched.Shutdown(context.Background()) }()

	srv, err := server.New(server.Config{
		Workflows:       map[string]*domain.Workflow{wf.Name: wf},
		DefaultWorkflow: wf.Name,
		GitHubAdapter:   ghAdapter,
		Scheduler:       sched,
		Store:           st,
		EnqueueTimeout:  5 * time.Second,
		APIToken:        "api-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	sendHook := func(deliv string) string {
		body := []byte(`{"action":"synchronize","number":42,"installation":{"id":1},"repository":{"name":"mochi","owner":{"login":"shohei-yamagiwa"}},"pull_request":{"number":42,"draft":false}}`)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", deliv)
		req.Header.Set("X-Hub-Signature-256", signHMAC(hookSecret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var accepted struct {
			RunID string `json:"run_id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&accepted)
		return accepted.RunID
	}

	run1 := sendHook("deliv-1")
	<-firstHydrating

	// Send second (newer) webhook while first is still blocked in hydration.
	run2 := sendHook("deliv-2")
	var res2 *orchestrator.RunResult
	deadline := time.Now().Add(3 * time.Second)
	for {
		res2, err = sched.WaitForRun(context.Background(), run2)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("WaitForRun(run2) timed out: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res2.Run.Status != domain.RunStatusCompleted {
		t.Fatalf("run2 status = %q, want completed", res2.Run.Status)
	}

	// Now unblock first hydration and drain all background goroutines.
	releaseOnce.Do(func() { close(releaseFirst) })
	if err := srv.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// Newer run must remain Completed and stale run1 must never execute.
	got2, err := st.GetRun(context.Background(), run2)
	if err != nil {
		t.Fatalf("GetRun(run2): %v", err)
	}
	if got2.Status != domain.RunStatusCompleted {
		t.Fatalf("run2 status after late hydration = %q, want completed", got2.Status)
	}
	if _, err := st.GetRun(context.Background(), run1); err == nil {
		t.Fatalf("stale run1 should not have been enqueued/persisted after run2 arrived")
	}
	recLLM.mu.Lock()
	joined := strings.Join(recLLM.contents, "\n")
	recLLM.mu.Unlock()
	if strings.Contains(joined, "Older push") || !strings.Contains(joined, "Newer push") {
		t.Fatalf("expected only Newer push to run, got LLM prompts: %s", joined)
	}
}

func TestServer_RepoWorkflowModeByBranchAndCommentCommand(t *testing.T) {
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "mochi.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

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
      provider: openai
      model: stub
    sandbox:
      enabled: false
    tools: ["submit_output"]
synthesizer:
  prompt_file: "configs/prompts/synthesizer.md"
  llm:
    provider: openai
    model: stub
output:
  sinks:
    - type: console
`
	smallWF := `version: "1"
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
    prompt_file: "configs/prompts/correctness.md"
    llm:
      provider: openai
      model: stub
    sandbox:
      enabled: false
    tools: ["submit_output"]
synthesizer:
  prompt_file: "configs/prompts/synthesizer.md"
  llm:
    provider: openai
    model: stub
output:
  sinks:
    - type: console
`

	var (
		mu         sync.Mutex
		baseBranch = "develop"
		headBranch = "feature/small-fix"
	)

	ghMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bb, hb := baseBranch, headBranch
		mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/10":
			if strings.Contains(r.Header.Get("Accept"), "application/vnd.github.v3.diff") {
				_, _ = w.Write([]byte("diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1 +1,2 @@\n package main\n+var ok = true\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   10,
				"title":    "Branch routed PR",
				"state":    "open",
				"draft":    false,
				"merged":   false,
				"html_url": "https://github.com/shohei-yamagiwa/mochi/pull/10",
				"user":     map[string]any{"login": "shohei"},
				"base":     map[string]any{"ref": bb, "sha": "base10", "repo": map[string]any{"clone_url": "https://github.com/shohei-yamagiwa/mochi.git"}},
				"head":     map[string]any{"ref": hb, "sha": "head10"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/pulls/10/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"filename": "app.go", "status": "modified", "additions": 1, "patch": "@@ -1 +1,2 @@\n package main\n+var ok = true"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/contents/.mochi/workflows":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "pr-review.yaml", "path": ".mochi/workflows/pr-review.yaml", "type": "file"},
				{"name": "pr-review-small.yaml", "path": ".mochi/workflows/pr-review-small.yaml", "type": "file"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/contents/.mochi/workflows/pr-review.yaml":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(mainWF)),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/shohei-yamagiwa/mochi/contents/.mochi/workflows/pr-review-small.yaml":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte(smallWF)),
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
	hookSecret := "hook-secret"
	ghAdapter, err := ghinput.NewAdapter(ghinput.NewStaticClientFactory(ghClient, "tok"), ghinput.WithWebhookSecret(hookSecret))
	if err != nil {
		t.Fatal(err)
	}

	runner := &orchestrator.Runner{
		Store:              st,
		DefaultLLMProvider: &stubLLM{name: "openai"},
		PromptEngine:       config.NewPromptEngine(""),
	}
	sched := orchestrator.NewScheduler(runner, orchestrator.SchedulerConfig{MaxWorkers: 2})
	defer func() { _ = sched.Shutdown(context.Background()) }()

	srv, err := server.New(server.Config{
		Workflows:     nil, // Repo-workflow mode
		GitHubAdapter: ghAdapter,
		Scheduler:     sched,
		Store:         st,
		APIToken:      "api-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	sendComment := func(cmd string) int {
		body := []byte(`{"action":"created","installation":{"id":1},"repository":{"name":"mochi","owner":{"login":"shohei-yamagiwa"}},"sender":{"login":"shohei","type":"User"},"issue":{"number":10,"state":"open","pull_request":{"url":"https://api.github.com/repos/shohei-yamagiwa/mochi/pulls/10"}},"comment":{"body":"` + cmd + `","author_association":"OWNER"}}`)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "issue_comment")
		req.Header.Set("X-GitHub-Delivery", "deliv-comment")
		req.Header.Set("X-Hub-Signature-256", signHMAC(hookSecret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Custom comment command /mochi review-small on feature/small-fix -> develop must trigger pr-review-small.
	if status := sendComment("/mochi review-small"); status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	if err := srv.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var runs []*domain.RunRecord
	for time.Now().Before(deadline) {
		runs, err = st.ListRuns(context.Background(), store.RunFilter{Limit: 10})
		if err == nil && len(runs) == 1 && runs[0].Status == domain.RunStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(runs) != 1 || runs[0].WorkflowName != "pr-review-small" || runs[0].Status != domain.RunStatusCompleted {
		t.Fatalf("expected 1 completed pr-review-small run, got %+v", runs)
	}

	// Now PR is develop -> main and pull_request.opened should trigger pr-review only.
	mu.Lock()
	baseBranch = "main"
	headBranch = "develop"
	mu.Unlock()

	prBody := []byte(`{"action":"opened","number":10,"installation":{"id":1},"repository":{"name":"mochi","owner":{"login":"shohei-yamagiwa"}},"pull_request":{"number":10,"draft":false}}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewReader(prBody))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "deliv-pr")
	req.Header.Set("X-Hub-Signature-256", signHMAC(hookSecret, prBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("PR webhook = %v, err=%v", resp, err)
	}
	_ = resp.Body.Close()

	if err := srv.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, err = st.ListRuns(context.Background(), store.RunFilter{Limit: 10})
		if err == nil && len(runs) == 2 && runs[0].Status == domain.RunStatusCompleted && runs[1].Status == domain.RunStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 total runs, got %d", len(runs))
	}
	seenWF := map[string]bool{}
	for _, r := range runs {
		seenWF[r.WorkflowName] = true
		if r.Status != domain.RunStatusCompleted {
			t.Fatalf("run %s (%s) status = %q, want completed", r.RunID, r.WorkflowName, r.Status)
		}
	}
	if !seenWF["pr-review-small"] || !seenWF["pr-review"] {
		t.Fatalf("expected both pr-review-small and pr-review runs, got %v", seenWF)
	}
}

func TestServer_RepoCredentialsCRUDAndStrictPerRepoExecution(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "mochi.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer st.Close()

	cipher, err := credentials.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	credSvc, err := credentials.NewService(st, cipher)
	if err != nil {
		t.Fatal(err)
	}

	// Ensure server-level LLM env vars are ignored in server mode.
	t.Setenv("GEMINI_API_KEY", "global-server-key-should-be-ignored")

	wf, promptDir := setupTestWorkflow(t)
	wf.Agents[0].LLM.Provider = "gemini"
	wf.Synthesizer.LLM.Provider = "gemini"

	runner := &orchestrator.Runner{
		Store:               st,
		PromptEngine:        config.NewPromptEngine(promptDir),
		LLMRegistryResolver: credSvc.RunnerResolver(),
	}
	sched := orchestrator.NewScheduler(runner, orchestrator.SchedulerConfig{MaxWorkers: 1})
	defer func() { _ = sched.Shutdown(context.Background()) }()

	srv, err := server.New(server.Config{
		Workflows:         map[string]*domain.Workflow{wf.Name: wf},
		DefaultWorkflow:   wf.Name,
		Scheduler:         sched,
		Store:             st,
		CredentialService: credSvc,
		TriggerSecret:     "trig-secret",
		APIToken:          "api-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. Unauthenticated access must fail with 401.
	resp, err := http.Get(ts.URL + "/api/v1/repos/shohei-yamagiwa/mochi/credentials")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without APIToken, got %v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// 2. PUT credential stores encrypted key and never returns plaintext api_key.
	putReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/repos/Shohei-Yamagiwa/Mochi/credentials/gemini",
		strings.NewReader(`{"api_key":"AIza-secret-repo-key"}`))
	putReq.Header.Set("Authorization", "Bearer api-secret")
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil || putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT credential = %v, err=%v", putResp, err)
	}
	putBodyBytes, _ := io.ReadAll(putResp.Body)
	_ = putResp.Body.Close()
	if strings.Contains(string(putBodyBytes), "AIza-secret-repo-key") {
		t.Fatalf("PUT response leaked api_key: %s", string(putBodyBytes))
	}

	// 3. GET credentials lists summaries without api_key.
	getReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/repos/shohei-yamagiwa/mochi/credentials", nil)
	getReq.Header.Set("Authorization", "Bearer api-secret")
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET credentials = %v, err=%v", getResp, err)
	}
	getBodyBytes, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	if strings.Contains(string(getBodyBytes), "AIza-secret-repo-key") {
		t.Fatalf("GET response leaked api_key: %s", string(getBodyBytes))
	}
	var listPayload struct {
		Credentials []*domain.RepoCredentialSummary `json:"credentials"`
	}
	if err := json.Unmarshal(getBodyBytes, &listPayload); err != nil {
		t.Fatal(err)
	}
	if len(listPayload.Credentials) != 1 || listPayload.Credentials[0].Provider != "gemini" || !listPayload.Credentials[0].HasAPIKey {
		t.Fatalf("unexpected credential summary list: %+v", listPayload.Credentials)
	}

	// 4. DELETE removes credential; second DELETE returns 404.
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/repos/shohei-yamagiwa/mochi/credentials/gemini", nil)
	delReq.Header.Set("Authorization", "Bearer api-secret")
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil || delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE credential = %v, err=%v", delResp, err)
	}
	_ = delResp.Body.Close()

	delReq2, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/repos/shohei-yamagiwa/mochi/credentials/gemini", nil)
	delReq2.Header.Set("Authorization", "Bearer api-secret")
	delResp2, err := http.DefaultClient.Do(delReq2)
	if err != nil || delResp2.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE credential = %v, want 404, err=%v", delResp2, err)
	}
	_ = delResp2.Body.Close()

	// 5. Triggering run for a repo with no stored credential must fail strictly (not fall back to GEMINI_API_KEY env).
	trigReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/triggers/pr-review",
		strings.NewReader(`{"repo_url":"https://github.com/shohei-yamagiwa/mochi.git","repo_owner":"shohei-yamagiwa","repo_name":"mochi","ref":"main"}`))
	trigReq.Header.Set("Authorization", "Bearer trig-secret")
	trigResp, err := http.DefaultClient.Do(trigReq)
	if err != nil || trigResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST trigger = %v, err=%v", trigResp, err)
	}
	var accepted struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(trigResp.Body).Decode(&accepted)
	_ = trigResp.Body.Close()

	if _, waitErr := sched.WaitForRun(ctx, accepted.RunID); waitErr == nil || !strings.Contains(waitErr.Error(), "missing gemini API key") {
		t.Fatalf("WaitForRun error = %v, want missing gemini API key error", waitErr)
	}
}
