package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/config"
	"github.com/shohei-yamagiwa/mochi/internal/credentials"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	cliinput "github.com/shohei-yamagiwa/mochi/internal/input/cli"
	ghinput "github.com/shohei-yamagiwa/mochi/internal/input/github"
	whinput "github.com/shohei-yamagiwa/mochi/internal/input/webhook"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/llm/anthropic"
	"github.com/shohei-yamagiwa/mochi/internal/llm/gemini"
	"github.com/shohei-yamagiwa/mochi/internal/llm/openai"
	"github.com/shohei-yamagiwa/mochi/internal/orchestrator"
	"github.com/shohei-yamagiwa/mochi/internal/output"
	"github.com/shohei-yamagiwa/mochi/internal/output/console"
	ghoutput "github.com/shohei-yamagiwa/mochi/internal/output/github"
	"github.com/shohei-yamagiwa/mochi/internal/output/jsonfile"
	whoutput "github.com/shohei-yamagiwa/mochi/internal/output/webhook"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox/daytona"
	"github.com/shohei-yamagiwa/mochi/internal/server"
	"github.com/shohei-yamagiwa/mochi/internal/store"
	"github.com/shohei-yamagiwa/mochi/internal/store/sqlite"
	"github.com/shohei-yamagiwa/mochi/internal/tools"
	"github.com/spf13/cobra"
)

const defaultWorkflowPath = ".mochi/workflows/pr-review.yaml"

// errChangesRequested signals exit code 2 when --fail-on-changes is set.
var errChangesRequested = errors.New("review verdict requested changes")

func main() {
	rootCmd := newRootCmd()
	if err := rootCmd.Execute(); err != nil {
		if errors.Is(err, errChangesRequested) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "mochi",
		Short:         "Mochi multi-agent code review orchestrator",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(
		newServeCmd(),
		newRunCmd(),
		newInspectCmd(),
		newValidateCmd(),
		newCredentialsCmd(),
	)
	return root
}

func newServeCmd() *cobra.Command {
	var (
		addr           string
		workflowPaths  []string
		baseDir        string
		dbPath         string
		workers        int
		queueSize      int
		webhookSecret  string
		triggerSecret  string
		apiToken       string
		masterKey      string
		insecureNoAuth bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Mochi webhook and inspection HTTP server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))

			var (
				workflows       map[string]*domain.Workflow
				defaultWorkflow string
			)
			resolvedBase := strings.TrimSpace(baseDir)
			if len(workflowPaths) > 0 {
				workflows = make(map[string]*domain.Workflow, len(workflowPaths))
				resolvedBase = resolveBaseDir(baseDir, workflowPaths[0])
				for _, p := range workflowPaths {
					bd := resolveBaseDir(baseDir, p)
					wf, err := config.LoadWorkflowFile(p, bd)
					if err != nil {
						return err
					}
					workflows[wf.Name] = wf
					if defaultWorkflow == "" {
						defaultWorkflow = wf.Name
					}
				}
			}

			ghClients, err := buildGitHubClientFactoryFromEnv(false)
			if err != nil {
				return err
			}
			if ghClients == nil && strings.TrimSpace(webhookSecret) != "" {
				return fmt.Errorf("GITHUB_WEBHOOK_SECRET is set, but GitHub App/token credentials (MOCHI_GITHUB_APP_ID or GITHUB_TOKEN) are missing")
			}

			if err := validateServeAuthConfig(ghClients != nil, webhookSecret, triggerSecret, apiToken, insecureNoAuth); err != nil {
				return err
			}
			if insecureNoAuth {
				logger.Warn("starting server with --insecure-no-auth enabled; unauthenticated access is permitted when auth secrets are unset",
					"webhook_secret_set", strings.TrimSpace(webhookSecret) != "",
					"trigger_secret_set", strings.TrimSpace(triggerSecret) != "",
					"api_token_set", strings.TrimSpace(apiToken) != "",
				)
			}

			cipher, err := credentials.NewCipherFromString(masterKey)
			if err != nil {
				return fmt.Errorf("init credential cipher: %w", err)
			}

			st, err := sqlite.Open(ctx, dbPath)
			if err != nil {
				return fmt.Errorf("open sqlite store %q: %w", dbPath, err)
			}
			defer st.Close()

			credSvc, err := credentials.NewService(st, cipher)
			if err != nil {
				return err
			}

			var sbProvider sandbox.Provider
			if envAny("DAYTONA_API_KEY", "DAYTONA_JWT_TOKEN") != "" {
				p, err := daytona.NewProvider(daytona.Config{})
				if err != nil {
					return fmt.Errorf("init daytona provider: %w", err)
				}
				sbProvider = p
			}

			var ghAdapter *ghinput.Adapter
			var overrideLoader orchestrator.RepoOverrideLoader
			if ghClients != nil {
				ghOpts := []ghinput.Option{
					ghinput.WithWebhookSecret(webhookSecret),
					ghinput.WithWorkflowName(defaultWorkflow),
				}
				if insecureNoAuth && strings.TrimSpace(webhookSecret) == "" {
					ghOpts = append(ghOpts, ghinput.WithAllowInsecureNoWebhookSecret(true))
				}
				ghAdapter, err = ghinput.NewAdapter(ghClients, ghOpts...)
				if err != nil {
					return err
				}
				overrideLoader = ghinput.NewRepoOverrideLoader(ghClients, true)
			}

			sinkReg, err := buildSinkRegistry(ghClients, cmd.OutOrStdout())
			if err != nil {
				return err
			}

			toolReg, err := tools.NewRegistry()
			if err != nil {
				return err
			}

			runner := &orchestrator.Runner{
				Store:               st,
				SandboxProvider:     sbProvider,
				LLMRegistryResolver: credSvc.RunnerResolver(),
				ToolRegistry:        toolReg,
				PromptEngine:        config.NewPromptEngine(resolvedBase),
				SinkRegistry:        sinkReg,
				RepoOverrideLoader:  overrideLoader,
				Logger:              logger,
			}

			sched := orchestrator.NewScheduler(runner, orchestrator.SchedulerConfig{
				MaxWorkers: workers,
				QueueSize:  queueSize,
			})

			srv, err := server.New(server.Config{
				Workflows:           workflows,
				DefaultWorkflow:     defaultWorkflow,
				GitHubAdapter:       ghAdapter,
				WebhookAdapter:      whinput.NewAdapter(defaultWorkflow),
				Scheduler:           sched,
				Store:               st,
				CredentialService:   credSvc,
				TriggerSecret:       triggerSecret,
				APIToken:            apiToken,
				AllowInsecureNoAuth: insecureNoAuth,
				Logger:              logger,
			})
			if err != nil {
				_ = sched.Shutdown(context.Background())
				return err
			}

			httpSrv := &http.Server{
				Addr:              addr,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
			}

			mode := "repo-workflows"
			if len(workflows) > 0 {
				mode = "static"
			}
			errCh := make(chan error, 1)
			go func() {
				logger.Info("mochi server listening", "addr", addr, "mode", mode, "workflows", len(workflows))
				if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					errCh <- err
					return
				}
				errCh <- nil
			}()

			select {
			case err := <-errCh:
				_ = sched.Shutdown(context.Background())
				return err
			case <-ctx.Done():
				logger.Info("shutting down mochi server")
			}

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			var errs []error
			if err := httpSrv.Shutdown(shutdownCtx); err != nil {
				errs = append(errs, err)
			}
			if err := srv.Shutdown(shutdownCtx); err != nil {
				errs = append(errs, err)
			}
			if err := sched.Shutdown(shutdownCtx); err != nil {
				errs = append(errs, err)
			}
			if err := <-errCh; err != nil {
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", envDefault("MOCHI_ADDR", ":8080"), "HTTP listen address")
	cmd.Flags().StringSliceVarP(&workflowPaths, "workflow", "w", splitEnvList(os.Getenv("MOCHI_WORKFLOW")), "Optional static workflow YAML file(s) to load (defaults to per-repo .mochi/workflows/*.yaml)")
	cmd.Flags().StringVar(&baseDir, "base-dir", "", "Base directory for resolving server prompt files (auto-detected if empty)")
	cmd.Flags().StringVar(&dbPath, "db", envDefault("MOCHI_DB_PATH", "mochi.db"), "SQLite database path")
	cmd.Flags().IntVar(&workers, "workers", 4, "Concurrent workflow worker count")
	cmd.Flags().IntVar(&queueSize, "queue-size", 128, "Scheduler queue size")
	cmd.Flags().StringVar(&webhookSecret, "webhook-secret", os.Getenv("GITHUB_WEBHOOK_SECRET"), "GitHub webhook HMAC-SHA256 secret")
	cmd.Flags().StringVar(&triggerSecret, "trigger-secret", os.Getenv("MOCHI_TRIGGER_SECRET"), "Bearer/HMAC secret for /api/v1/triggers")
	cmd.Flags().StringVar(&apiToken, "api-token", os.Getenv("MOCHI_API_TOKEN"), "Bearer token for /api/v1/runs and /api/v1/repos/.../credentials endpoints")
	cmd.Flags().StringVar(&masterKey, "master-key", os.Getenv("MOCHI_MASTER_KEY"), "32-byte AES-256 master key (64 hex chars or base64) for per-repository LLM API keys")
	cmd.Flags().BoolVar(&insecureNoAuth, "insecure-no-auth", false, "Allow starting the server without webhook/trigger/API auth secrets (unsafe for production)")
	return cmd
}

func validateServeAuthConfig(ghEnabled bool, webhookSecret, triggerSecret, apiToken string, insecureNoAuth bool) error {
	if insecureNoAuth {
		return nil
	}
	if ghEnabled && strings.TrimSpace(webhookSecret) == "" {
		return fmt.Errorf("refusing to start with GitHub webhooks enabled and empty webhook secret: set GITHUB_WEBHOOK_SECRET/--webhook-secret or pass --insecure-no-auth")
	}
	if strings.TrimSpace(triggerSecret) == "" && strings.TrimSpace(apiToken) == "" {
		return fmt.Errorf("refusing to start without trigger or API authentication secrets: set MOCHI_TRIGGER_SECRET/--trigger-secret and/or MOCHI_API_TOKEN/--api-token, or pass --insecure-no-auth")
	}
	return nil
}

func newRunCmd() *cobra.Command {
	var (
		workflowPath  string
		baseDir       string
		prRef         string
		repoPath      string
		baseRef       string
		headRef       string
		dbPath        string
		dryRun        bool
		jsonOut       bool
		outputPath    string
		postGitHub    bool
		failOnChanges bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a workflow once against a local git diff or GitHub PR",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			resolvedBase := resolveBaseDir(baseDir, workflowPath)
			wf, err := config.LoadWorkflowFile(workflowPath, resolvedBase)
			if err != nil {
				return err
			}

			var ghClients ghinput.ClientFactory
			if strings.TrimSpace(prRef) != "" || postGitHub {
				ghClients, err = buildGitHubClientFactoryFromEnv(true)
				if err != nil {
					return err
				}
			}

			adapter := cliinput.NewAdapter()
			task, err := adapter.BuildTaskContext(ctx, &cliinput.RunRequest{
				WorkflowName:  wf.Name,
				PRRef:         prRef,
				RepoPath:      repoPath,
				BaseRef:       baseRef,
				HeadRef:       headRef,
				GitHubClients: ghClients,
			})
			if err != nil {
				return err
			}

			if dryRun {
				return printDryRun(cmd.OutOrStdout(), wf, task, resolvedBase, jsonOut)
			}

			var st store.Store
			if strings.TrimSpace(dbPath) != "" {
				sqliteStore, err := sqlite.Open(ctx, dbPath)
				if err != nil {
					return fmt.Errorf("open sqlite store %q: %w", dbPath, err)
				}
				defer sqliteStore.Close()
				st = sqliteStore
			}

			llmReg, err := buildLLMRegistryFromEnv()
			if err != nil {
				return err
			}
			for _, a := range wf.Agents {
				if _, ok := llmReg.Get(a.LLM.Provider); !ok {
					return fmt.Errorf("agent %q requires llm provider %q, which is not configured in environment variables", a.Role, a.LLM.Provider)
				}
			}
			if _, ok := llmReg.Get(wf.Synthesizer.LLM.Provider); !ok {
				return fmt.Errorf("synthesizer requires llm provider %q, which is not configured in environment variables", wf.Synthesizer.LLM.Provider)
			}

			var sbProvider sandbox.Provider
			if needsSandbox(wf) {
				p, err := daytona.NewProvider(daytona.Config{})
				if err != nil {
					return fmt.Errorf("init daytona sandbox provider: %w", err)
				}
				sbProvider = p
			}

			toolReg, err := tools.NewRegistry()
			if err != nil {
				return err
			}

			// In CLI mode, suppress GitHub PR review posting unless --post-github is explicitly set.
			execWf := *wf
			if !postGitHub {
				filtered := make([]domain.SinkConfig, 0, len(wf.Output.Sinks)+1)
				for _, sc := range wf.Output.Sinks {
					if sc.Type == "github_pr_review" {
						continue
					}
					filtered = append(filtered, sc)
				}
				if len(filtered) == 0 && !jsonOut {
					filtered = append(filtered, domain.SinkConfig{
						Type:                   "console",
						IncludeCoverageSummary: true,
					})
				}
				execWf.Output.Sinks = filtered
			}
			if strings.TrimSpace(outputPath) != "" {
				execWf.Output.Sinks = append(execWf.Output.Sinks, domain.SinkConfig{
					Type:    "json_file",
					Options: map[string]any{"path": outputPath},
				})
			}

			sinkReg, err := buildSinkRegistry(ghClients, cmd.OutOrStdout())
			if err != nil {
				return err
			}

			var overrideLoader orchestrator.RepoOverrideLoader
			if ghClients != nil {
				overrideLoader = ghinput.NewRepoOverrideLoader(ghClients, true)
			}

			runner := &orchestrator.Runner{
				Store:              st,
				SandboxProvider:    sbProvider,
				LLMRegistry:        llmReg,
				ToolRegistry:       toolReg,
				PromptEngine:       config.NewPromptEngine(resolvedBase),
				SinkRegistry:       sinkReg,
				RepoOverrideLoader: overrideLoader,
				Logger:             slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelWarn})),
			}

			res, err := runner.Execute(ctx, task, &execWf)
			if err != nil {
				return err
			}

			if jsonOut && len(res.ArtifactJSON) > 0 {
				var pretty any
				if json.Unmarshal(res.ArtifactJSON, &pretty) == nil {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					if err := enc.Encode(pretty); err != nil {
						return err
					}
				} else {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", string(res.ArtifactJSON))
				}
			}

			if failOnChanges && res.Report != nil && res.Report.Verdict == output.VerdictRequestChanges {
				return errChangesRequested
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&workflowPath, "workflow", "w", defaultWorkflowPath, "Workflow YAML file path")
	cmd.Flags().StringVar(&baseDir, "base-dir", "", "Base directory for resolving server prompt files (auto-detected if empty)")
	cmd.Flags().StringVar(&prRef, "pr", "", "GitHub PR URL or owner/repo#N")
	cmd.Flags().StringVar(&repoPath, "repo", ".", "Local git repository path (used when --pr is not specified)")
	cmd.Flags().StringVar(&baseRef, "base", "", "Base git ref for local diff")
	cmd.Flags().StringVar(&headRef, "head", "HEAD", "Head git ref for local diff")
	cmd.Flags().StringVar(&dbPath, "db", envDefault("MOCHI_DB_PATH", "mochi.db"), "SQLite database path (empty disables persistence)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Resolve workflow and diff TaskContext without running LLMs or sandboxes")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print structured JSON output")
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "Write final JSON artifact to file")
	cmd.Flags().BoolVar(&postGitHub, "post-github", false, "Allow github_pr_review sink to post comments/check runs when --pr is specified")
	cmd.Flags().BoolVar(&failOnChanges, "fail-on-changes", false, "Exit with code 2 when verdict is REQUEST_CHANGES")
	return cmd
}

func newInspectCmd() *cobra.Command {
	var (
		dbPath   string
		workflow string
		status   string
		limit    int
		jsonOut  bool
	)

	cmd := &cobra.Command{
		Use:   "inspect [run_id]",
		Short: "Inspect persisted workflow runs, LLM turns, tool calls, and diff coverage",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := sqlite.Open(ctx, dbPath)
			if err != nil {
				return fmt.Errorf("open sqlite store %q: %w", dbPath, err)
			}
			defer st.Close()

			if len(args) == 0 {
				runs, err := st.ListRuns(ctx, store.RunFilter{
					WorkflowName: workflow,
					Status:       domain.RunStatus(status),
					Limit:        limit,
				})
				if err != nil {
					return err
				}
				if jsonOut {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(runs)
				}
				return printRunList(cmd.OutOrStdout(), runs)
			}

			detail, err := st.GetRunDetail(ctx, strings.TrimSpace(args[0]))
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(detail)
			}
			return printRunDetail(cmd.OutOrStdout(), detail)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", envDefault("MOCHI_DB_PATH", "mochi.db"), "SQLite database path")
	cmd.Flags().StringVar(&workflow, "workflow", "", "Filter run list by workflow name")
	cmd.Flags().StringVar(&status, "status", "", "Filter run list by status")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of runs to list")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print inspection output as JSON")
	return cmd
}

func newValidateCmd() *cobra.Command {
	var (
		baseDir    string
		runtime    bool
		serverMode bool
	)

	cmd := &cobra.Command{
		Use:   "validate [workflow_paths...]",
		Short: "Validate workflow YAML files, tools, and prompt templates",
		RunE: func(cmd *cobra.Command, args []string) error {
			if serverMode || (runtime && len(args) == 0) {
				if err := validateServerRuntime(); err != nil {
					return fmt.Errorf("server runtime configuration: %w", err)
				}
				if len(args) == 0 {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "valid: server runtime configuration (GitHub auth, webhook/API secrets, Daytona, MOCHI_MASTER_KEY)")
					return nil
				}
			}

			paths := args
			if len(paths) == 0 {
				paths = discoverLocalWorkflows()
			}

			toolReg, err := tools.NewRegistry()
			if err != nil {
				return err
			}

			for _, wfPath := range paths {
				resolvedBase := resolveBaseDir(baseDir, wfPath)
				wf, err := config.LoadWorkflowFile(wfPath, resolvedBase)
				if err != nil {
					return err
				}

				engine := config.NewPromptEngineWithRoots(resolvedBase, resolvedBase)
				for _, a := range wf.Agents {
					if _, err := toolReg.ScopeForRole(a.Tools); err != nil {
						return fmt.Errorf("%s: agent %q: %w", wfPath, a.Role, err)
					}
					raw, err := engine.ReadPrompt(a.PromptFile, a.PromptFromRepo)
					if err != nil {
						return fmt.Errorf("%s: read prompt %q: %w", wfPath, a.PromptFile, err)
					}
					if err := engine.ValidateTemplate(a.PromptFile, string(raw)); err != nil {
						return fmt.Errorf("%s: %w", wfPath, err)
					}
				}
				synthRaw, err := engine.ReadPrompt(wf.Synthesizer.PromptFile, wf.Synthesizer.PromptFromRepo)
				if err != nil {
					return fmt.Errorf("%s: read synthesizer prompt %q: %w", wfPath, wf.Synthesizer.PromptFile, err)
				}
				if err := engine.ValidateTemplate(wf.Synthesizer.PromptFile, string(synthRaw)); err != nil {
					return fmt.Errorf("%s: %w", wfPath, err)
				}
				if runtime && !serverMode {
					if err := validateRuntimeWorkflow(wf); err != nil {
						return fmt.Errorf("%s: runtime configuration: %w", wfPath, err)
					}
				}

				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "valid: %s (%s, %d agents, %d sinks)\n",
					wf.Name, wfPath, len(wf.Agents), len(wf.Output.Sinks))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&baseDir, "base-dir", "", "Base directory for resolving prompt_file paths (auto-detected if empty)")
	cmd.Flags().BoolVar(&runtime, "runtime", false, "Check runtime environment configuration locally (no external API calls)")
	cmd.Flags().BoolVar(&serverMode, "server", false, "Check server deployment environment (GitHub auth, webhook/API secrets, Daytona, MOCHI_MASTER_KEY)")
	return cmd
}

func discoverLocalWorkflows() []string {
	var found []string
	for _, pat := range []string{filepath.Join(".mochi", "workflows", "*.yaml"), filepath.Join(".mochi", "workflows", "*.yml")} {
		matches, err := filepath.Glob(pat)
		if err == nil {
			found = append(found, matches...)
		}
	}
	if len(found) == 0 {
		return []string{defaultWorkflowPath}
	}
	sort.Strings(found)
	return found
}

func resolveBaseDir(explicit, workflowPath string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	wfDir := filepath.Dir(workflowPath)
	// Walk upward from workflow file directory to locate prompt files or project root (.mochi / configs/prompts / go.mod).
	cur := wfDir
	for range 5 {
		if _, err := os.Stat(filepath.Join(cur, ".mochi")); err == nil {
			return cur
		}
		if _, err := os.Stat(filepath.Join(cur, "configs", "prompts")); err == nil {
			return cur
		}
		if _, err := os.Stat(filepath.Join(cur, "go.mod")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "."
}

func buildLLMRegistryFromEnv() (*llm.Registry, error) {
	reg := llm.NewRegistry()
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		p, err := anthropic.NewProvider()
		if err != nil {
			return nil, fmt.Errorf("init anthropic provider: %w", err)
		}
		_ = reg.Register(p)
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		p, err := openai.NewProvider()
		if err != nil {
			return nil, fmt.Errorf("init openai provider: %w", err)
		}
		_ = reg.Register(p)
	}
	if os.Getenv("GEMINI_API_KEY") != "" || os.Getenv("GOOGLE_API_KEY") != "" {
		p, err := gemini.NewProvider()
		if err != nil {
			return nil, fmt.Errorf("init gemini provider: %w", err)
		}
		_ = reg.Register(p)
	}
	if base := strings.TrimSpace(os.Getenv("OPENAI_COMPAT_BASE_URL")); base != "" {
		p, err := openai.NewCompatProvider(
			openai.WithBaseURL(base),
			openai.WithAPIKey(envAny("OPENAI_COMPAT_API_KEY", "OPENAI_API_KEY")),
		)
		if err != nil {
			return nil, fmt.Errorf("init openai_compat provider: %w", err)
		}
		_ = reg.Register(p)
	}
	return reg, nil
}

func buildGitHubClientFactoryFromEnv(required bool) (ghinput.ClientFactory, error) {
	rawAppID := strings.TrimSpace(os.Getenv("MOCHI_GITHUB_APP_ID"))
	privKey := os.Getenv("MOCHI_GITHUB_PRIVATE_KEY")
	privKeyPath := strings.TrimSpace(os.Getenv("MOCHI_GITHUB_PRIVATE_KEY_PATH"))
	token := envAny("GITHUB_TOKEN", "GH_TOKEN")
	baseURL := strings.TrimSpace(os.Getenv("GITHUB_API_URL"))

	var appID int64
	if rawAppID != "" {
		parsed, err := strconv.ParseInt(rawAppID, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid MOCHI_GITHUB_APP_ID %q", rawAppID)
		}
		appID = parsed
	}

	if appID == 0 && token == "" {
		if required {
			return nil, fmt.Errorf("github authentication requires MOCHI_GITHUB_APP_ID+MOCHI_GITHUB_PRIVATE_KEY(_PATH) or GITHUB_TOKEN")
		}
		return nil, nil
	}

	return ghinput.NewClientFactory(ghinput.AppConfig{
		AppID:          appID,
		PrivateKey:     []byte(privKey),
		PrivateKeyPath: privKeyPath,
		StaticToken:    token,
		BaseURL:        baseURL,
	})
}

func buildSinkRegistry(ghClients ghinput.ClientFactory, out io.Writer) (*output.Registry, error) {
	reg := output.NewRegistry()
	_ = reg.Register(console.NewSink(out))
	_ = reg.Register(whoutput.NewSink(whoutput.Config{}))
	_ = reg.Register(jsonfile.NewSink(""))
	if ghClients != nil {
		ghSink, err := ghoutput.NewSink(ghClients)
		if err != nil {
			return nil, err
		}
		_ = reg.Register(ghSink)
	}
	return reg, nil
}

func needsSandbox(wf *domain.Workflow) bool {
	if wf == nil {
		return false
	}
	for _, a := range wf.Agents {
		if a.Sandbox.Enabled {
			return true
		}
	}
	return false
}

func printDryRun(w io.Writer, wf *domain.Workflow, task *domain.TaskContext, baseDir string, jsonOut bool) error {
	if jsonOut {
		payload := map[string]any{
			"dry_run":  true,
			"workflow": wf.Name,
			"task":     config.NewPromptTaskContext(task),
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	fmt.Fprintf(w, "Dry Run: workflow=%s trigger=%s\n", wf.Name, task.TriggerType)
	if task.Changeset != nil {
		cs := task.Changeset
		fmt.Fprintf(w, "Changeset: %s/%s (base=%s head=%s, changed_files=%d)\n",
			cs.RepoOwner, cs.RepoName, cs.BaseBranch, cs.HeadBranch, len(cs.ChangedFiles))
		for _, cf := range cs.ChangedFiles {
			fmt.Fprintf(w, "  - %s (%s, +%d/-%d, hunks=%d)\n",
				cf.Path, cf.Status, cf.Additions, cf.Deletions, len(cf.ChangedHunks))
		}
	}

	engine := config.NewPromptEngine(baseDir)
	promptData := config.NewPromptData(task, wf, nil)
	fmt.Fprintf(w, "Specialist Agents (%d):\n", len(wf.Agents))
	for _, role := range wf.Agents {
		roleCopy := role
		promptData.Role = &roleCopy
		rendered, err := engine.RenderRole(role, promptData)
		status := fmt.Sprintf("rendered (%d bytes)", len(rendered))
		if err != nil {
			status = fmt.Sprintf("render error: %v", err)
		}
		fmt.Fprintf(w, "  - %s (%s/%s) -> %s\n", role.Role, role.LLM.Provider, role.LLM.Model, status)
	}
	return nil
}

func printRunList(w io.Writer, runs []*domain.RunRecord) error {
	if len(runs) == 0 {
		_, err := fmt.Fprintln(w, "No workflow runs found.")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN ID\tWORKFLOW\tTRIGGER\tSTATUS\tSTARTED")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.RunID, r.WorkflowName, r.TriggerType, r.Status, r.StartedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

func printRunDetail(w io.Writer, detail *store.RunDetail) error {
	if detail == nil || detail.Run == nil {
		return fmt.Errorf("run detail is empty")
	}
	r := detail.Run
	fmt.Fprintf(w, "Run ID:   %s\n", r.RunID)
	fmt.Fprintf(w, "Workflow: %s (trigger=%s, status=%s)\n", r.WorkflowName, r.TriggerType, r.Status)
	if r.DedupKey != "" {
		fmt.Fprintf(w, "DedupKey: %s\n", r.DedupKey)
	}
	fmt.Fprintf(w, "Started:  %s\n", r.StartedAt.Format(time.RFC3339))
	if r.CompletedAt != nil {
		fmt.Fprintf(w, "Finished: %s (%s)\n", r.CompletedAt.Format(time.RFC3339), r.CompletedAt.Sub(r.StartedAt).Round(time.Millisecond))
	}
	if r.ErrorMessage != "" {
		fmt.Fprintf(w, "Error:    %s\n", r.ErrorMessage)
	}

	if cs := detail.Changeset; cs != nil {
		fmt.Fprintln(w, "\nChangeset:")
		if cs.PRNumber > 0 {
			fmt.Fprintf(w, "  PR #%d — %s (%s/%s)\n", cs.PRNumber, cs.Title, cs.RepoOwner, cs.RepoName)
		} else {
			fmt.Fprintf(w, "  Repo: %s/%s — %s\n", cs.RepoOwner, cs.RepoName, cs.Title)
		}
		fmt.Fprintf(w, "  Commits: %s..%s (%s <- %s), %d changed files\n",
			shortSHA(cs.BaseSHA), shortSHA(cs.HeadSHA), cs.BaseBranch, cs.HeadBranch, len(cs.ChangedFiles))
	}

	if len(detail.AgentRuns) > 0 {
		fmt.Fprintln(w, "\nAgent Runs:")
		for _, ar := range detail.AgentRuns {
			if ar.AgentRun == nil {
				continue
			}
			a := ar.AgentRun
			fmt.Fprintf(w, "  - %-26s status=%-10s model=%s/%s turns=%d tokens=%d/%d\n",
				a.Role, a.Status, a.Provider, a.Model, a.TurnsUsed, a.InputTokens, a.OutputTokens)
			for _, turn := range ar.LLMTurns {
				var calls []json.RawMessage
				if len(turn.ToolCallsJSON) > 0 {
					_ = json.Unmarshal(turn.ToolCallsJSON, &calls)
				}
				fmt.Fprintf(w, "      turn #%d: in=%d out=%d tool_calls=%d (%dms)\n",
					turn.TurnIndex, turn.InputTokens, turn.OutputTokens, len(calls), turn.LatencyMS)
			}
			for _, te := range ar.ToolExecutions {
				fmt.Fprintf(w, "      tool %s exit=%d (%dms, accessed=%d)\n",
					te.ToolName, te.ExitCode, te.DurationMS, len(te.AccessedFiles))
			}
		}
	}

	if cov := detail.Coverage; cov != nil {
		fmt.Fprintf(w, "\nCoverage Matrix (%d/%d files, %.0f%% hunks):\n",
			cov.ReviewedChangedFiles, cov.TotalChangedFiles, cov.HunkCoverageRatio*100)
		if len(cov.UnreviewedFiles) > 0 {
			fmt.Fprintf(w, "  Unreviewed files: %s\n", strings.Join(cov.UnreviewedFiles, ", "))
		}
		if len(cov.FileDetails) > 0 {
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "  FILE\tSTATUS\tINSPECTED BY\tHUNKS\tUNCOVERED\tFINDINGS")
			for _, fd := range cov.FileDetails {
				agents := "-"
				if len(fd.InspectedByAgents) > 0 {
					agents = strings.Join(fd.InspectedByAgents, ",")
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%d\t%d\n",
					fd.Path, fd.ChangeStatus, agents, len(fd.ChangedHunks), len(fd.UncoveredHunks), fd.FindingsCount)
			}
			_ = tw.Flush()
		}
	}

	if len(r.FinalArtifactJSON) > 0 {
		var rep output.ReviewReport
		if err := json.Unmarshal(r.FinalArtifactJSON, &rep); err == nil {
			fmt.Fprintf(w, "\nFinal Report (%s):\n  %s\n  Findings: %d\n",
				rep.Verdict, rep.Summary, len(rep.Findings))
		}
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envAny(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func splitEnvList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
