package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

// PromptData exposes canonical workflow and task context variables to prompt templates.
type PromptData struct {
	Task           *domain.TaskContext
	Workflow       *domain.Workflow
	Role           *domain.RoleConfig
	AgentSummaries map[string]string
	Coverage       *domain.CoverageReport
	Extra          map[string]any
}

// PromptEngine loads and renders Go text/template prompt files safely.
type PromptEngine struct {
	baseDir string
	funcs   template.FuncMap
}

// NewPromptEngine constructs a PromptEngine rooted at baseDir for resolving relative prompt file paths.
func NewPromptEngine(baseDir string) *PromptEngine {
	return &PromptEngine{
		baseDir: baseDir,
		funcs: template.FuncMap{
			"shortSHA": func(s string) string {
				if len(s) > 7 {
					return s[:7]
				}
				return s
			},
			"truncate": func(maxLen int, s string) string {
				if maxLen <= 0 || len(s) <= maxLen {
					return s
				}
				return s[:maxLen] + "...(truncated)"
			},
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

// RenderFile reads a prompt template relative to baseDir and renders it with data.
func (e *PromptEngine) RenderFile(promptPath string, data PromptData) (string, error) {
	if err := ensureSafeRelativePath(promptPath); err != nil {
		return "", err
	}
	fullPath := filepath.Join(e.baseDir, filepath.FromSlash(promptPath))
	raw, err := os.ReadFile(fullPath)
	if err != nil {
		return "", fmt.Errorf("read prompt template %q: %w", fullPath, err)
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
