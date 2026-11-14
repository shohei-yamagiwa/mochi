package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"unicode/utf8"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

// PromptWorkspaceSpec is a secret-free projection of domain.WorkspaceSpec for prompt templates.
// It omits CloneToken and strips any URL userinfo from RepoURL so credentials can never be interpolated into LLM prompts.
type PromptWorkspaceSpec struct {
	RepoURL    string `json:"repo_url"`
	Ref        string `json:"ref"`
	BaseRef    string `json:"base_ref"`
	Snapshot   string `json:"snapshot,omitempty"`
	Image      string `json:"image,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

// PromptTaskContext is a secret-free projection of domain.TaskContext exposed to prompt templates.
// Input adapters and callers must never place credentials or tokens in TaskContext.Variables.
type PromptTaskContext struct {
	RunID        string                `json:"run_id"`
	WorkflowName string                `json:"workflow_name"`
	TriggerType  string                `json:"trigger_type"`
	DedupKey     string                `json:"dedup_key"`
	Workspace    PromptWorkspaceSpec   `json:"workspace"`
	Changeset    *domain.ChangesetSpec `json:"changeset"`
	Variables    map[string]any        `json:"variables"`
}

// NewPromptTaskContext constructs a secret-free PromptTaskContext from a domain.TaskContext.
func NewPromptTaskContext(task *domain.TaskContext) *PromptTaskContext {
	if task == nil {
		return nil
	}
	var vars map[string]any
	if task.Variables != nil {
		vars = maps.Clone(task.Variables)
	}
	return &PromptTaskContext{
		RunID:        task.RunID,
		WorkflowName: task.WorkflowName,
		TriggerType:  task.TriggerType,
		DedupKey:     task.DedupKey,
		Workspace: PromptWorkspaceSpec{
			RepoURL:    sanitizeRepoURL(task.Workspace.RepoURL),
			Ref:        task.Workspace.Ref,
			BaseRef:    task.Workspace.BaseRef,
			Snapshot:   task.Workspace.Snapshot,
			Image:      task.Workspace.Image,
			Dockerfile: task.Workspace.Dockerfile,
		},
		Changeset: task.Changeset,
		Variables: vars,
	}
}

func sanitizeRepoURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.User != nil {
		u.User = nil
		return u.String()
	}
	return raw
}

// PromptData exposes canonical, secret-free workflow and task context variables to prompt templates.
type PromptData struct {
	Task           *PromptTaskContext
	Workflow       *domain.Workflow
	Role           *domain.RoleConfig
	AgentSummaries map[string]string
	Coverage       *domain.CoverageReport
	Extra          map[string]any
}

// NewPromptData builds a secret-free PromptData from a domain.TaskContext.
func NewPromptData(task *domain.TaskContext, wf *domain.Workflow, role *domain.RoleConfig) PromptData {
	return PromptData{
		Task:     NewPromptTaskContext(task),
		Workflow: wf,
		Role:     role,
	}
}

// PromptEngine loads and renders Go text/template prompt files using separate server and repo roots.
// Both roots are opened via os.Root to prevent path traversal and symlink escapes.
type PromptEngine struct {
	serverDir string
	repoDir   string
	funcs     template.FuncMap
}

// NewPromptEngine constructs a PromptEngine rooted at serverDir for server-owned prompt files.
func NewPromptEngine(serverDir string) *PromptEngine {
	return NewPromptEngineWithRoots(serverDir, "")
}

// NewPromptEngineWithRoots constructs a PromptEngine with separate roots for server prompts and
// repository-overridden prompts (which must be fetched from the repository's base branch).
func NewPromptEngineWithRoots(serverDir, repoDir string) *PromptEngine {
	return &PromptEngine{
		serverDir: serverDir,
		repoDir:   repoDir,
		funcs: template.FuncMap{
			"shortSHA": func(s string) string {
				if len(s) > 7 {
					return s[:7]
				}
				return s
			},
			"truncate": truncateRunes,
			"join": func(sep string, items []string) string {
				return strings.Join(items, sep)
			},
			"toJSON": func(v any) (string, error) {
				b, err := json.MarshalIndent(v, "", "  ")
				if err != nil {
					return "", err
				}
				return string(b), nil
			},
			"indent": func(spaces int, s string) string {
				if spaces <= 0 || s == "" {
					return s
				}
				pad := strings.Repeat(" ", spaces)
				lines := strings.Split(s, "\n")
				for i, l := range lines {
					if l != "" {
						lines[i] = pad + l
					}
				}
				return strings.Join(lines, "\n")
			},
			"default": func(fallback, val any) any {
				if val == nil {
					return fallback
				}
				if str, ok := val.(string); ok && str == "" {
					return fallback
				}
				return val
			},
		},
	}
}

// WithRepoDir returns a shallow copy of the PromptEngine configured to read repository-owned
// prompts (under .mochi/) from repoDir.
func (e *PromptEngine) WithRepoDir(repoDir string) *PromptEngine {
	if e == nil {
		return NewPromptEngineWithRoots(".", repoDir)
	}
	return NewPromptEngineWithRoots(e.serverDir, repoDir)
}

// RenderRole selects the server or repository root based on role.PromptFromRepo and renders its prompt.
func (e *PromptEngine) RenderRole(role domain.RoleConfig, data PromptData) (string, error) {
	return e.RenderPrompt(role.PromptFile, role.PromptFromRepo, data)
}

// RenderSynthesizer selects the server or repository root based on synth.PromptFromRepo and renders its prompt.
func (e *PromptEngine) RenderSynthesizer(synth domain.SynthesizerConfig, data PromptData) (string, error) {
	return e.RenderPrompt(synth.PromptFile, synth.PromptFromRepo, data)
}

// RenderFile reads a server-owned prompt template relative to serverDir and renders it with data.
func (e *PromptEngine) RenderFile(promptPath string, data PromptData) (string, error) {
	return e.RenderPrompt(promptPath, false, data)
}

// RenderRepoFile reads a repository-supplied prompt template (under .mochi/ from the base branch)
// relative to repoDir and renders it with data.
func (e *PromptEngine) RenderRepoFile(promptPath string, data PromptData) (string, error) {
	return e.RenderPrompt(promptPath, true, data)
}

// RenderPrompt reads a prompt template from either serverDir (fromRepo=false) or repoDir (fromRepo=true)
// using os.Root confinement so symlinks cannot escape the designated root.
func (e *PromptEngine) RenderPrompt(promptPath string, fromRepo bool, data PromptData) (string, error) {
	rootDir := e.serverDir
	if fromRepo {
		if err := ensureSafeRepoPromptPath(promptPath); err != nil {
			return "", err
		}
		if strings.TrimSpace(e.repoDir) == "" {
			return "", fmt.Errorf("cannot render repo prompt %q: repo prompt root is not configured", promptPath)
		}
		rootDir = e.repoDir
	} else {
		if err := ensureSafeRelativePath(promptPath); err != nil {
			return "", err
		}
	}

	raw, err := readFileInRoot(rootDir, promptPath)
	if err != nil {
		return "", err
	}
	return e.RenderTemplate(promptPath, string(raw), data)
}

// RenderTemplate parses and executes an inline Go text/template string with strict missingkey=error handling.
func (e *PromptEngine) RenderTemplate(name, tmplText string, data PromptData) (string, error) {
	tmpl, err := template.New(name).Funcs(e.funcs).Option("missingkey=error").Parse(tmplText)
	if err != nil {
		return "", fmt.Errorf("parse prompt template %q: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute prompt template %q: %w", name, err)
	}
	return buf.String(), nil
}

// ValidateTemplate checks that a template parses without syntax errors.
func (e *PromptEngine) ValidateTemplate(name, tmplText string) error {
	if _, err := template.New(name).Funcs(e.funcs).Option("missingkey=error").Parse(tmplText); err != nil {
		return fmt.Errorf("invalid prompt template %q: %w", name, err)
	}
	return nil
}

func readFileInRoot(rootDir, promptPath string) ([]byte, error) {
	if strings.TrimSpace(rootDir) == "" {
		return nil, fmt.Errorf("prompt root directory is empty")
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("open prompt root %q: %w", rootDir, err)
	}
	defer root.Close()

	cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(promptPath)))
	raw, err := root.ReadFile(cleanRel)
	if err != nil {
		return nil, fmt.Errorf("read prompt template %q in root %q: %w", cleanRel, rootDir, err)
	}
	return raw, nil
}

func truncateRunes(maxRunes int, s string) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	var count int
	for idx := range s {
		if count == maxRunes {
			return s[:idx] + "...(truncated)"
		}
		count++
	}
	return s
}
