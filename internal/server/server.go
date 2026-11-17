package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/shohei-yamagiwa/mochi/internal/credentials"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	ghinput "github.com/shohei-yamagiwa/mochi/internal/input/github"
	whinput "github.com/shohei-yamagiwa/mochi/internal/input/webhook"
	"github.com/shohei-yamagiwa/mochi/internal/orchestrator"
	"github.com/shohei-yamagiwa/mochi/internal/store"
)

const (
	defaultMaxBodyBytes   int64 = 10 << 20 // 10 MiB
	defaultEnqueueTimeout       = 30 * time.Second
	defaultListLimit            = 50
	maxListLimit                = 500
)

// Config configures the HTTP server for webhooks, triggers, health probes, and run inspection.
type Config struct {
	Workflows           map[string]*domain.Workflow
	DefaultWorkflow     string
	GitHubAdapter       *ghinput.Adapter
	WebhookAdapter      *whinput.Adapter
	Scheduler           *orchestrator.Scheduler
	Store               store.Store
	CredentialService   *credentials.Service
	TriggerSecret       string
	APIToken            string
	AllowInsecureNoAuth bool
	MaxBodyBytes        int64
	EnqueueTimeout      time.Duration
	Logger              *slog.Logger
}

type hydrationInFlight struct {
	seq    uint64
	cancel context.CancelFunc
}

// Server exposes Mochi's HTTP endpoints over chi/v5.
type Server struct {
	cfg      Config
	router   *chi.Mux
	logger   *slog.Logger
	bgWG     sync.WaitGroup
	bgCtx    context.Context
	bgCancel context.CancelFunc

	hydrationMu      sync.Mutex
	webhookSeq       uint64
	latestSeqByDedup map[string]uint64
	hydratingByDedup map[string]hydrationInFlight
}

// New constructs a Server with all health, webhook, trigger, and run-inspection routes registered.
func New(cfg Config) (*Server, error) {
	if cfg.Scheduler == nil {
		return nil, fmt.Errorf("server: scheduler must not be nil")
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.EnqueueTimeout <= 0 {
		cfg.EnqueueTimeout = defaultEnqueueTimeout
	}
	if cfg.WebhookAdapter == nil {
		cfg.WebhookAdapter = whinput.NewAdapter(cfg.DefaultWorkflow)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:              cfg,
		logger:           logger,
		bgCtx:            bgCtx,
		bgCancel:         bgCancel,
		latestSeqByDedup: make(map[string]uint64),
		hydratingByDedup: make(map[string]hydrationInFlight),
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.Post("/webhooks/github", s.handleGitHubWebhook)
	r.Post("/api/v1/webhooks/github", s.handleGitHubWebhook)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/triggers/{workflow}", s.handleTriggerWorkflow)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAPIToken)
			r.Get("/runs", s.handleListRuns)
			r.Get("/runs/{run_id}", s.handleGetRun)
			r.Get("/runs/{run_id}/coverage", s.handleGetRunCoverage)
			r.Post("/runs/{run_id}/cancel", s.handleCancelRun)

			r.Get("/repos/{owner}/{repo}/credentials", s.handleListRepoCredentials)
			r.Put("/repos/{owner}/{repo}/credentials/{provider}", s.handlePutRepoCredential)
			r.Delete("/repos/{owner}/{repo}/credentials/{provider}", s.handleDeleteRepoCredential)
		})
	})

	s.router = r
	return s, nil
}

// Handler returns the underlying http.Handler.
func (s *Server) Handler() http.Handler {
	return s.router
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

// Drain waits for in-flight asynchronous webhook enqueue tasks to complete before returning.
// If ctx expires before completion, Drain cancels pending background fetches and returns ctx.Err().
func (s *Server) Drain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.bgWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.bgCancel()
		<-done
		return ctx.Err()
	}
}

// Shutdown cancels pending background webhook fetches and waits for workers to exit.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.Drain(ctx)
	s.bgCancel()
	return err
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler unavailable")
		return
	}
	if s.cfg.Store != nil {
		if _, err := s.cfg.Store.ListRuns(r.Context(), store.RunFilter{Limit: 1}); err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("store not ready: %v", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.cfg.GitHubAdapter == nil {
		writeError(w, http.StatusServiceUnavailable, "github webhook adapter is not configured")
		return
	}

	body, err := readBoundedBody(w, r, s.cfg.MaxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "webhook payload exceeds maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if err := s.cfg.GitHubAdapter.VerifySignature(body, sig); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}

	eventType := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	deliveryID := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if eventType == "" {
		writeError(w, http.StatusBadRequest, "missing X-GitHub-Event header")
		return
	}

	if strings.TrimSpace(r.URL.Query().Get("workflow")) == "" && len(s.cfg.Workflows) == 0 {
		s.handleRepoWorkflowGitHubWebhook(w, body, eventType, deliveryID)
		return
	}

	wf, err := s.resolveGitHubWorkflow(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var triggers *domain.TriggerConfig
	if wf != nil {
		t := wf.Input.Triggers
		triggers = &t
	}
	target, err := s.cfg.GitHubAdapter.ParseWebhookEvent(eventType, body, triggers)
	if err != nil {
		if errors.Is(err, ghinput.ErrIgnoredEvent) {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":      "ignored",
				"delivery_id": deliveryID,
				"reason":      err.Error(),
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	target.WorkflowName = wf.Name
	target.Triggers = triggers
	runID := newRunID()

	dedupKey := ""
	if target.Owner != "" && target.Repo != "" && target.PRNumber > 0 {
		dedupKey = fmt.Sprintf("github:%s/%s:pr:%d", target.Owner, target.Repo, target.PRNumber)
	}
	hydrateCtx, hydrateCancel := context.WithTimeout(s.bgCtx, s.cfg.EnqueueTimeout)
	seq := s.registerWebhookHydration(dedupKey, hydrateCancel)

	// Acknowledge immediately with 202 Accepted and asynchronously fetch PR metadata/diff + enqueue.
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer hydrateCancel()
		defer s.finishWebhookHydration(dedupKey, seq)

		task, buildErr := s.cfg.GitHubAdapter.BuildTaskContext(hydrateCtx, target)
		if buildErr != nil {
			if errors.Is(buildErr, ghinput.ErrIgnoredEvent) {
				s.logger.Info("ignored github pr event after hydration", "delivery_id", deliveryID, "pr", target.PRNumber, "reason", buildErr)
				return
			}
			if s.isWebhookSuperseded(dedupKey, seq) && errors.Is(buildErr, context.Canceled) {
				s.logger.Info("cancelled superseded github pr webhook hydration", "run_id", runID, "delivery_id", deliveryID, "pr", target.PRNumber, "seq", seq)
				return
			}
			s.logger.Error("failed to hydrate github pr task context", "delivery_id", deliveryID, "pr", target.PRNumber, "error", buildErr)
			return
		}
		if s.isWebhookSuperseded(dedupKey, seq) {
			s.logger.Info("skipping enqueue for superseded github pr webhook", "run_id", runID, "delivery_id", deliveryID, "pr", target.PRNumber, "seq", seq)
			return
		}
		task.RunID = runID
		task.DedupSequence = seq
		if task.WorkflowName == "" {
			task.WorkflowName = wf.Name
		}

		if _, enqErr := s.cfg.Scheduler.Enqueue(hydrateCtx, task, wf); enqErr != nil {
			if errors.Is(enqErr, orchestrator.ErrRunSuperseded) {
				s.logger.Info("github pr webhook superseded before enqueue", "run_id", runID, "delivery_id", deliveryID, "pr", target.PRNumber, "seq", seq)
				return
			}
			s.logger.Error("failed to enqueue github pr workflow run", "run_id", runID, "pr", target.PRNumber, "error", enqErr)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":      "accepted",
		"run_id":      runID,
		"workflow":    wf.Name,
		"pr_number":   target.PRNumber,
		"delivery_id": deliveryID,
	})
}

func (s *Server) handleRepoWorkflowGitHubWebhook(w http.ResponseWriter, body []byte, eventType, deliveryID string) {
	cand, err := s.cfg.GitHubAdapter.ParseWebhookCandidate(eventType, body)
	if err != nil {
		if errors.Is(err, ghinput.ErrIgnoredEvent) {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":      "ignored",
				"delivery_id": deliveryID,
				"reason":      err.Error(),
			})
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	hydrationDedupKey := ""
	if cand.Owner != "" && cand.Repo != "" && cand.PRNumber > 0 {
		hydrationDedupKey = fmt.Sprintf("github:%s/%s:pr:%d", cand.Owner, cand.Repo, cand.PRNumber)
	}
	hydrateCtx, hydrateCancel := context.WithTimeout(s.bgCtx, s.cfg.EnqueueTimeout)
	seq := s.registerWebhookHydration(hydrationDedupKey, hydrateCancel)

	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer hydrateCancel()
		defer s.finishWebhookHydration(hydrationDedupKey, seq)

		repoTasks, buildErr := s.cfg.GitHubAdapter.BuildTaskContextsForRepoWorkflows(hydrateCtx, cand)
		if buildErr != nil {
			if errors.Is(buildErr, ghinput.ErrIgnoredEvent) {
				s.logger.Info("ignored github pr event in repo-workflow mode", "delivery_id", deliveryID, "pr", cand.PRNumber, "reason", buildErr)
				return
			}
			if s.isWebhookSuperseded(hydrationDedupKey, seq) && errors.Is(buildErr, context.Canceled) {
				s.logger.Info("cancelled superseded github repo-workflow hydration", "delivery_id", deliveryID, "pr", cand.PRNumber, "seq", seq)
				return
			}
			s.logger.Error("failed to resolve repo workflows for github pr", "delivery_id", deliveryID, "pr", cand.PRNumber, "error", buildErr)
			return
		}

		cleanupRemaining := func(startIdx int) {
			for i := startIdx; i < len(repoTasks); i++ {
				if rt := repoTasks[i]; rt != nil && rt.Task != nil && rt.Task.PromptDir != "" {
					_ = os.RemoveAll(rt.Task.PromptDir)
				}
			}
		}

		if s.isWebhookSuperseded(hydrationDedupKey, seq) {
			cleanupRemaining(0)
			s.logger.Info("skipping enqueue for superseded github repo-workflow webhook", "delivery_id", deliveryID, "pr", cand.PRNumber, "seq", seq)
			return
		}

		for i, rt := range repoTasks {
			if rt == nil || rt.Task == nil || rt.Workflow == nil {
				continue
			}
			if s.isWebhookSuperseded(hydrationDedupKey, seq) {
				cleanupRemaining(i)
				return
			}
			rt.Task.RunID = newRunID()
			rt.Task.DedupSequence = seq
			if _, enqErr := s.cfg.Scheduler.Enqueue(hydrateCtx, rt.Task, rt.Workflow); enqErr != nil {
				if rt.Task.PromptDir != "" {
					_ = os.RemoveAll(rt.Task.PromptDir)
				}
				if errors.Is(enqErr, orchestrator.ErrRunSuperseded) {
					s.logger.Info("github repo-workflow run superseded before enqueue", "workflow", rt.Workflow.Name, "delivery_id", deliveryID, "pr", cand.PRNumber, "seq", seq)
					continue
				}
				s.logger.Error("failed to enqueue github repo-workflow run", "workflow", rt.Workflow.Name, "run_id", rt.Task.RunID, "pr", cand.PRNumber, "error", enqErr)
			}
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":      "accepted",
		"mode":        "repo_workflows",
		"pr_number":   cand.PRNumber,
		"delivery_id": deliveryID,
	})
}

func (s *Server) registerWebhookHydration(dedupKey string, cancel context.CancelFunc) uint64 {
	s.hydrationMu.Lock()
	s.webhookSeq++
	seq := s.webhookSeq
	var prevCancel context.CancelFunc
	if dedupKey != "" {
		if prev, ok := s.hydratingByDedup[dedupKey]; ok && prev.cancel != nil {
			prevCancel = prev.cancel
		}
		s.latestSeqByDedup[dedupKey] = seq
		s.hydratingByDedup[dedupKey] = hydrationInFlight{seq: seq, cancel: cancel}
	}
	s.hydrationMu.Unlock()
	if prevCancel != nil {
		prevCancel()
	}
	return seq
}

func (s *Server) finishWebhookHydration(dedupKey string, seq uint64) {
	if dedupKey == "" {
		return
	}
	s.hydrationMu.Lock()
	if cur, ok := s.hydratingByDedup[dedupKey]; ok && cur.seq == seq {
		delete(s.hydratingByDedup, dedupKey)
	}
	s.hydrationMu.Unlock()
}

func (s *Server) isWebhookSuperseded(dedupKey string, seq uint64) bool {
	if dedupKey == "" || seq == 0 {
		return false
	}
	s.hydrationMu.Lock()
	latest := s.latestSeqByDedup[dedupKey]
	s.hydrationMu.Unlock()
	return seq < latest
}

func (s *Server) handleTriggerWorkflow(w http.ResponseWriter, r *http.Request) {
	body, err := readBoundedBody(w, r, s.cfg.MaxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "trigger payload exceeds maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	if !s.authorizeTrigger(r, body) {
		writeError(w, http.StatusUnauthorized, "unauthorized trigger request")
		return
	}

	workflowName := strings.TrimSpace(chi.URLParam(r, "workflow"))
	wf, ok := s.lookupWorkflow(workflowName)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("workflow %q not found", workflowName))
		return
	}

	task, err := s.cfg.WebhookAdapter.BuildTaskContext(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	task.WorkflowName = wf.Name
	if strings.TrimSpace(task.RunID) == "" {
		task.RunID = newRunID()
	}

	runID, err := s.cfg.Scheduler.Enqueue(r.Context(), task, wf)
	if err != nil {
		if errors.Is(err, orchestrator.ErrSchedulerClosed) {
			writeError(w, http.StatusServiceUnavailable, "scheduler is shutting down")
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("enqueue run: %v", err))
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":   "accepted",
		"run_id":   runID,
		"workflow": wf.Name,
	})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "store is not configured")
		return
	}

	q := r.URL.Query()
	filter := store.RunFilter{
		WorkflowName: strings.TrimSpace(q.Get("workflow")),
		DedupKey:     strings.TrimSpace(q.Get("dedup_key")),
		Status:       domain.RunStatus(strings.TrimSpace(q.Get("status"))),
		Limit:        defaultListLimit,
	}
	if rawLimit := strings.TrimSpace(q.Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if parsed > maxListLimit {
			parsed = maxListLimit
		}
		filter.Limit = parsed
	}

	runs, err := s.cfg.Store.ListRuns(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list runs: %v", err))
		return
	}
	if runs == nil {
		runs = []*domain.RunRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs": runs,
	})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "store is not configured")
		return
	}
	runID := strings.TrimSpace(chi.URLParam(r, "run_id"))
	detail, err := s.cfg.Store.GetRunDetail(r.Context(), runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("run %q not found", runID))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("get run detail: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleGetRunCoverage(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "store is not configured")
		return
	}
	runID := strings.TrimSpace(chi.URLParam(r, "run_id"))
	cov, err := s.cfg.Store.GetCoverageReport(r.Context(), runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("coverage report for run %q not found", runID))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("get coverage report: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, cov)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(chi.URLParam(r, "run_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		raw, err := readBoundedBody(w, r, 64<<10)
		if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "cancelled via API"
	}

	if s.cfg.Scheduler.CancelRun(runID, reason) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": "cancelling",
			"run_id": runID,
		})
		return
	}

	if s.cfg.Store != nil {
		rec, err := s.cfg.Store.GetRun(r.Context(), runID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, fmt.Sprintf("run %q not found", runID))
				return
			}
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("lookup run: %v", err))
			return
		}
		writeError(w, http.StatusConflict, fmt.Sprintf("run %q is not active (status=%s)", runID, rec.Status))
		return
	}

	writeError(w, http.StatusNotFound, fmt.Sprintf("run %q not found", runID))
}

func (s *Server) handleListRepoCredentials(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CredentialService == nil {
		writeError(w, http.StatusServiceUnavailable, "credential service is not configured")
		return
	}
	owner, repo, err := credentials.NormalizeRepo(chi.URLParam(r, "owner"), chi.URLParam(r, "repo"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	creds, err := s.cfg.CredentialService.List(r.Context(), owner, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("list credentials: %v", err))
		return
	}
	if creds == nil {
		creds = []*domain.RepoCredentialSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"credentials": creds,
	})
}

func (s *Server) handlePutRepoCredential(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CredentialService == nil {
		writeError(w, http.StatusServiceUnavailable, "credential service is not configured")
		return
	}
	owner, repo, err := credentials.NormalizeRepo(chi.URLParam(r, "owner"), chi.URLParam(r, "repo"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	provider, err := credentials.ValidateProvider(chi.URLParam(r, "provider"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	raw, err := readBoundedBody(w, r, 64<<10)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "credential payload exceeds maximum size")
			return
		}
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req struct {
		APIKey  string `json:"api_key"`
		BaseURL string `json:"base_url"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid json body: %v", err))
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "api_key must not be empty")
		return
	}

	if err := s.cfg.CredentialService.Set(r.Context(), owner, repo, provider, req.APIKey, req.BaseURL); err != nil {
		if strings.Contains(err.Error(), "invalid base_url") || strings.Contains(err.Error(), "must not be empty") || strings.Contains(err.Error(), "unsupported provider") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("store credential: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "saved",
		"repo_owner":  owner,
		"repo_name":   repo,
		"provider":    provider,
		"base_url":    strings.TrimSpace(req.BaseURL),
		"has_api_key": true,
	})
}

func (s *Server) handleDeleteRepoCredential(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CredentialService == nil {
		writeError(w, http.StatusServiceUnavailable, "credential service is not configured")
		return
	}
	owner, repo, err := credentials.NormalizeRepo(chi.URLParam(r, "owner"), chi.URLParam(r, "repo"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	provider, err := credentials.ValidateProvider(chi.URLParam(r, "provider"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.cfg.CredentialService.Delete(r.Context(), owner, repo, provider); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("credential for %s/%s (%s) not found", owner, repo, provider))
			return
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("delete credential: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "deleted",
		"repo_owner": owner,
		"repo_name":  repo,
		"provider":   provider,
	})
}

func (s *Server) resolveGitHubWorkflow(r *http.Request) (*domain.Workflow, error) {
	if q := strings.TrimSpace(r.URL.Query().Get("workflow")); q != "" {
		wf, ok := s.lookupWorkflow(q)
		if !ok {
			return nil, fmt.Errorf("workflow %q not found", q)
		}
		return wf, nil
	}
	if s.cfg.DefaultWorkflow != "" {
		if wf, ok := s.lookupWorkflow(s.cfg.DefaultWorkflow); ok {
			return wf, nil
		}
	}
	if wf, ok := s.lookupWorkflow("pr-review"); ok {
		return wf, nil
	}
	for _, wf := range s.cfg.Workflows {
		if wf != nil && wf.Input.Adapter == "github_pr" {
			return wf, nil
		}
	}
	for _, wf := range s.cfg.Workflows {
		if wf != nil {
			return wf, nil
		}
	}
	return nil, fmt.Errorf("no workflow configured")
}

func (s *Server) lookupWorkflow(name string) (*domain.Workflow, bool) {
	if s.cfg.Workflows == nil {
		return nil, false
	}
	wf, ok := s.cfg.Workflows[strings.TrimSpace(name)]
	return wf, ok && wf != nil
}

func (s *Server) requireAPIToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIToken == "" {
			if s.cfg.AllowInsecureNoAuth {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !checkBearerToken(r.Header.Get("Authorization"), s.cfg.APIToken) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorizeTrigger(r *http.Request, body []byte) bool {
	if s.cfg.TriggerSecret != "" {
		if checkBearerToken(r.Header.Get("Authorization"), s.cfg.TriggerSecret) {
			return true
		}
		if sig := strings.TrimSpace(r.Header.Get("X-Mochi-Signature-256")); sig != "" {
			return verifyHMAC256(body, sig, s.cfg.TriggerSecret)
		}
		return false
	}
	if s.cfg.APIToken != "" {
		return checkBearerToken(r.Header.Get("Authorization"), s.cfg.APIToken)
	}
	return s.cfg.AllowInsecureNoAuth
}

func checkBearerToken(header, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	tok := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(tok), []byte(expected)) == 1
}

func verifyHMAC256(payload []byte, header, secret string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return hmac.Equal(got, mac.Sum(nil))
}

func readBoundedBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	defer limited.Close()
	return io.ReadAll(limited)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func newRunID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(buf[:])
}
