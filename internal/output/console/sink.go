package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/output"
)

const sinkName = "console"

// Sink implements output.OutputSink (and output.ConfigurableSink) by rendering
// the ReviewReport as readable Markdown to an io.Writer.
type Sink struct {
	mu  sync.Mutex
	w   io.Writer
	cfg domain.SinkConfig
}

var (
	_ output.OutputSink       = (*Sink)(nil)
	_ output.ConfigurableSink = (*Sink)(nil)
)

// NewSink creates a ConsoleSink writing Markdown reports to w (default os.Stdout).
func NewSink(w io.Writer) *Sink {
	if w == nil {
		w = os.Stdout
	}
	return &Sink{
		w: w,
		cfg: domain.SinkConfig{
			Type:                   sinkName,
			IncludeCoverageSummary: true,
		},
	}
}

// Name returns "console".
func (s *Sink) Name() string {
	return sinkName
}

// WithConfig returns a shallow clone configured with cfg while sharing the target writer.
func (s *Sink) WithConfig(cfg domain.SinkConfig) output.OutputSink {
	return &Sink{
		w:   s.w,
		cfg: cfg,
	}
}

// OnRunStarted is a no-op for the console sink.
func (s *Sink) OnRunStarted(_ context.Context, _ *domain.TaskContext) error {
	return nil
}

// Publish formats and writes the ReviewReport to the configured writer.
func (s *Sink) Publish(_ context.Context, task *domain.TaskContext, artifact json.RawMessage) error {
	if len(artifact) == 0 {
		return fmt.Errorf("console output sink: empty artifact")
	}
	var report output.ReviewReport
	if err := json.Unmarshal(artifact, &report); err != nil {
		// Fall back to printing raw artifact if it is not a ReviewReport.
		s.mu.Lock()
		defer s.mu.Unlock()
		_, writeErr := fmt.Fprintf(s.w, "%s\n", strings.TrimSpace(string(artifact)))
		return writeErr
	}

	rendered := RenderMarkdown(task, &report, s.cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(s.w, rendered)
	return err
}

// OnRunFailed writes failure context to the configured writer.
func (s *Sink) OnRunFailed(_ context.Context, task *domain.TaskContext, runErr error) error {
	runID := ""
	if task != nil {
		runID = task.RunID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := fmt.Fprintf(s.w, "## Mochi Run Failed (`%s`)\n\nError: %v\n", runID, runErr)
	return err
}

// RenderMarkdown formats a ReviewReport and optional TaskContext as Markdown.
func RenderMarkdown(task *domain.TaskContext, report *output.ReviewReport, cfg domain.SinkConfig) string {
	if report == nil {
		return ""
	}
	var b strings.Builder

	fmt.Fprintf(&b, "# Mochi Review Report — `%s`\n\n", report.Verdict)
	if task != nil {
		if task.RunID != "" {
			fmt.Fprintf(&b, "- **Run ID**: `%s`\n", task.RunID)
		}
		if task.WorkflowName != "" {
			fmt.Fprintf(&b, "- **Workflow**: `%s`\n", task.WorkflowName)
		}
		if task.Changeset != nil && task.Changeset.PRNumber > 0 {
			fmt.Fprintf(&b, "- **PR**: %s/%s#%d — %s\n",
				task.Changeset.RepoOwner, task.Changeset.RepoName, task.Changeset.PRNumber, task.Changeset.Title)
		}
		b.WriteByte('\n')
	}

	if summary := strings.TrimSpace(report.Summary); summary != "" {
		b.WriteString("## Summary\n\n")
		b.WriteString(summary)
		b.WriteString("\n\n")
	}

	if len(report.FailedAgents) > 0 {
		fmt.Fprintf(&b, "> **Partial Review**: Failed agents: `%s`\n\n", strings.Join(report.FailedAgents, "`, `"))
	}

	var kept []output.Finding
	dropped := 0
	for _, f := range report.Findings {
		if !output.SeverityMeetsMinimum(f.Severity, cfg.MinSeverity) {
			dropped++
			continue
		}
		if cfg.MinConfidence > 0 && f.Confidence < cfg.MinConfidence {
			dropped++
			continue
		}
		kept = append(kept, f)
	}

	fmt.Fprintf(&b, "## Findings (%d)\n\n", len(kept))
	if len(kept) == 0 {
		b.WriteString("_No findings matching configured thresholds._\n\n")
	} else {
		for i, f := range kept {
			loc := f.FilePath
			if f.StartLine > 0 {
				if f.EndLine > f.StartLine {
					loc = fmt.Sprintf("%s:%d-%d", f.FilePath, f.StartLine, f.EndLine)
				} else {
					loc = fmt.Sprintf("%s:%d", f.FilePath, f.StartLine)
				}
			}
			fmt.Fprintf(&b, "%d. **[%s]** `%s` — **%s**", i+1, strings.ToUpper(string(f.Severity)), loc, f.Title)
			if f.Category != "" || f.Confidence > 0 {
				fmt.Fprintf(&b, " _(%s, conf %.2f)_", f.Category, f.Confidence)
			}
			b.WriteByte('\n')
			if desc := strings.TrimSpace(f.Description); desc != "" {
				fmt.Fprintf(&b, "   %s\n", strings.ReplaceAll(desc, "\n", "\n   "))
			}
			if sug := strings.TrimRight(f.SuggestionCode, "\r\n"); strings.TrimSpace(sug) != "" {
				fmt.Fprintf(&b, "   ```suggestion\n   %s\n   ```\n", strings.ReplaceAll(sug, "\n", "\n   "))
			}
		}
		b.WriteByte('\n')
	}

	if dropped > 0 {
		fmt.Fprintf(&b, "_%d finding(s) filtered below severity=%q / confidence=%.2f._\n\n",
			dropped, cfg.MinSeverity, cfg.MinConfidence)
	}

	if report.TestExecution != nil {
		b.WriteString("## Test Execution\n\n")
		fmt.Fprintf(&b, "- **Ran**: %v | **Passed**: %v\n", report.TestExecution.Ran, report.TestExecution.Passed)
		if report.TestExecution.Summary != "" {
			fmt.Fprintf(&b, "- **Summary**: %s\n", report.TestExecution.Summary)
		}
		if len(report.TestExecution.FailedTests) > 0 {
			fmt.Fprintf(&b, "- **Failed tests**: `%s`\n", strings.Join(report.TestExecution.FailedTests, "`, `"))
		}
		b.WriteByte('\n')
	}

	if (cfg.IncludeCoverageSummary || cfg.Type == "") && report.Coverage != nil {
		cov := report.Coverage
		fmt.Fprintf(&b, "## Review Coverage (%d/%d files, %.0f%% hunks)\n\n",
			cov.ReviewedChangedFiles, cov.TotalChangedFiles, cov.HunkCoverageRatio*100)
		if len(cov.UnreviewedFiles) > 0 {
			fmt.Fprintf(&b, "- **Unreviewed files**: `%s`\n\n", strings.Join(cov.UnreviewedFiles, "`, `"))
		}
		if len(cov.FileDetails) > 0 {
			b.WriteString("| File | Status | Inspected By | Hunks | Uncovered | Findings |\n")
			b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
			for _, fd := range cov.FileDetails {
				agents := "-"
				if len(fd.InspectedByAgents) > 0 {
					agents = strings.Join(fd.InspectedByAgents, ", ")
				}
				fmt.Fprintf(&b, "| `%s` | %s | %s | %d | %d | %d |\n",
					fd.Path, fd.ChangeStatus, agents, len(fd.ChangedHunks), len(fd.UncoveredHunks), fd.FindingsCount)
			}
			b.WriteByte('\n')
		}
	}

	return b.String()
}
