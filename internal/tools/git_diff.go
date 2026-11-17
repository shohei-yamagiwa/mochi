package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
)

var diffHunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

type gitDiffArgs struct {
	Path    string `json:"path,omitempty" jsonschema:"Optional repository-relative file or directory path to filter the diff"`
	BaseRef string `json:"base_ref,omitempty" jsonschema:"Optional base git ref/SHA (defaults to PR base ref)"`
	HeadRef string `json:"head_ref,omitempty" jsonschema:"Optional head git ref/SHA (defaults to PR head ref)"`
}

type gitDiffTool struct {
	schema   *jsonschema.Schema
	maxBytes int
}

// NewGitDiffTool creates the git_diff sandbox tool.
func NewGitDiffTool() Tool {
	s, _ := jsonschema.For[gitDiffArgs](nil)
	return &gitDiffTool{
		schema:   s,
		maxBytes: defaultMaxOutputBytes,
	}
}

func (t *gitDiffTool) Name() string { return "git_diff" }

func (t *gitDiffTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        t.Name(),
		Description: "Inspect unified git diff between base and head commits inside the sandbox, optionally filtered by a repository-relative path.",
		Parameters:  t.schema,
	}
}

func (t *gitDiffTool) Execute(ctx context.Context, execCtx ExecutionContext, rawArgs json.RawMessage) (*ExecutionResult, error) {
	if execCtx.Sandbox == nil {
		return nil, fmt.Errorf("git_diff: sandbox is nil")
	}
	var args gitDiffArgs
	if len(strings.TrimSpace(string(rawArgs))) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return &ExecutionResult{
				Output:   fmt.Sprintf("invalid arguments: %v", err),
				ExitCode: 1,
			}, nil
		}
	}

	var cleanPath string
	if strings.TrimSpace(args.Path) != "" {
		p, err := normalizeRepoPath(args.Path, true)
		if err != nil {
			return &ExecutionResult{
				Output:   fmt.Sprintf("invalid path: %v", err),
				ExitCode: 1,
			}, nil
		}
		if p != "." {
			cleanPath = p
		}
	}

	baseRef := strings.TrimSpace(args.BaseRef)
	if baseRef == "" {
		baseRef = strings.TrimSpace(execCtx.BaseRef)
	}
	headRef := strings.TrimSpace(args.HeadRef)
	if headRef == "" {
		headRef = strings.TrimSpace(execCtx.HeadRef)
	}
	if err := validateGitRef(baseRef); err != nil {
		return &ExecutionResult{Output: fmt.Sprintf("invalid base_ref: %v", err), ExitCode: 1}, nil
	}
	if err := validateGitRef(headRef); err != nil {
		return &ExecutionResult{Output: fmt.Sprintf("invalid head_ref: %v", err), ExitCode: 1}, nil
	}

	cmdParts := []string{"git diff"}
	switch {
	case baseRef != "" && headRef != "":
		cmdParts = append(cmdParts, shellQuote(baseRef+"..."+headRef))
	case baseRef != "":
		cmdParts = append(cmdParts, shellQuote(baseRef))
	}
	if cleanPath != "" {
		cmdParts = append(cmdParts, "--", shellQuote(cleanPath))
	}

	maxBytes := t.maxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxOutputBytes
	}

	res, err := execCtx.Sandbox.Exec(ctx, sandbox.ExecRequest{
		Command: strings.Join(cmdParts, " "),
	})
	if err != nil {
		return nil, fmt.Errorf("git_diff: %w", err)
	}
	if res.ExitCode != 0 {
		errOut := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
		return &ExecutionResult{
			Output:   truncateHeadTail(fmt.Sprintf("git_diff failed (exit=%d): %s", res.ExitCode, errOut), maxBytes),
			ExitCode: res.ExitCode,
		}, nil
	}

	diffText := res.Stdout
	out := strings.TrimSpace(diffText)
	if out == "" {
		out = "(empty diff)"
	}

	formattedOut, head, tail, truncated := truncateHeadTailWithParts(out, maxBytes)

	var accessed []domain.FileAccessRecord
	if !truncated {
		if cleanPath != "" {
			accessed = append(accessed, domain.FileAccessRecord{
				AgentRole: execCtx.AgentRole,
				Path:      cleanPath,
				Kind:      domain.FileAccessDiff,
			})
		} else {
			for _, p := range extractDiffFiles(diffText) {
				accessed = append(accessed, domain.FileAccessRecord{
					AgentRole: execCtx.AgentRole,
					Path:      p,
					Kind:      domain.FileAccessDiff,
				})
			}
		}
	} else {
		accessed = extractSurvivingDiffAccess(head, tail, cleanPath, execCtx.AgentRole)
	}

	return &ExecutionResult{
		Output:        formattedOut,
		ExitCode:      0,
		AccessedFiles: accessed,
	}, nil
}

func validateGitRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("ref %q must not start with '-'", ref)
	}
	if strings.ContainsAny(ref, " \t\n\r;") {
		return fmt.Errorf("ref %q contains invalid characters", ref)
	}
	return nil
}

func extractDiffFiles(diff string) []string {
	var files []string
	seen := make(map[string]struct{})
	for line := range strings.SplitSeq(diff, "\n") {
		if p := parseDiffFileLine(line); p != "" {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				files = append(files, p)
			}
		}
	}
	return files
}

func parseDiffFileLine(line string) string {
	if strings.HasPrefix(line, "diff --git ") {
		rest := strings.TrimPrefix(line, "diff --git ")
		if idx := strings.Index(rest, " b/"); idx >= 0 {
			p := strings.TrimSpace(rest[idx+3:])
			if p != "" && p != "/dev/null" {
				return p
			}
		}
	} else if strings.HasPrefix(line, "+++ b/") {
		p := strings.TrimSpace(strings.TrimPrefix(line, "+++ b/"))
		if p != "" && p != "/dev/null" {
			return p
		}
	}
	return ""
}

func parseDiffHunkLine(line string) (domain.LineRange, bool) {
	m := diffHunkHeaderRe.FindStringSubmatch(line)
	if len(m) == 0 {
		return domain.LineRange{}, false
	}
	start, err := strconv.Atoi(m[1])
	if err != nil || start <= 0 {
		return domain.LineRange{}, false
	}
	count := 1
	if m[2] != "" {
		if c, err := strconv.Atoi(m[2]); err == nil {
			count = c
		}
	}
	end := start
	if count > 1 {
		end = start + count - 1
	}
	return domain.LineRange{StartLine: start, EndLine: end}, true
}

// extractSurvivingDiffAccess emits FileAccessDiff records only for files and hunks that survived
// head/tail truncation, avoiding false coverage for files or hunks dropped in the truncated middle.
func extractSurvivingDiffAccess(head, tail, fallbackPath, agentRole string) []domain.FileAccessRecord {
	type fileSection struct {
		path  string
		hunks []domain.LineRange
	}

	parseSections := func(chunk string, initialPath string) []fileSection {
		var sections []fileSection
		curPath := initialPath
		for line := range strings.SplitSeq(chunk, "\n") {
			if strings.HasPrefix(line, "diff --git ") {
				p := parseDiffFileLine(line)
				curPath = p
				if p != "" {
					sections = append(sections, fileSection{path: p})
				}
				continue
			}
			if strings.HasPrefix(line, "+++ b/") {
				p := parseDiffFileLine(line)
				if p != "" {
					curPath = p
					if len(sections) == 0 || sections[len(sections)-1].path != p {
						sections = append(sections, fileSection{path: p})
					}
				}
				continue
			}
			if lr, ok := parseDiffHunkLine(line); ok && curPath != "" {
				if len(sections) == 0 || sections[len(sections)-1].path != curPath {
					sections = append(sections, fileSection{path: curPath})
				}
				sections[len(sections)-1].hunks = append(sections[len(sections)-1].hunks, lr)
			}
		}
		return sections
	}

	var records []domain.FileAccessRecord
	headSections := parseSections(head, fallbackPath)
	for i, sec := range headSections {
		isLastSectionInHead := i == len(headSections)-1
		if !isLastSectionInHead {
			// Every file before the final section in head was completely output before the next file began.
			records = append(records, domain.FileAccessRecord{
				AgentRole: agentRole,
				Path:      sec.path,
				Kind:      domain.FileAccessDiff,
			})
			continue
		}
		// The final section in head was interrupted by truncation; only hunks prior to the last hunk are guaranteed complete.
		completeHunks := sec.hunks
		if len(completeHunks) > 1 {
			completeHunks = completeHunks[:len(completeHunks)-1]
		} else {
			// A single hunk in the last head section was cut by truncation and is conservatively omitted.
			completeHunks = nil
		}
		for _, h := range completeHunks {
			hCopy := h
			records = append(records, domain.FileAccessRecord{
				AgentRole: agentRole,
				Path:      sec.path,
				Kind:      domain.FileAccessDiff,
				LineRange: &hCopy,
			})
		}
	}

	tailSections := parseSections(tail, "")
	for i, sec := range tailSections {
		// If a file's `diff --git` header starts inside tail (and is not a mid-file continuation at index 0 without diff --git),
		// its entire diff through the end of output survived intact.
		if i > 0 || strings.HasPrefix(strings.TrimLeft(tail, "\r\n"), "diff --git ") {
			records = append(records, domain.FileAccessRecord{
				AgentRole: agentRole,
				Path:      sec.path,
				Kind:      domain.FileAccessDiff,
			})
			continue
		}
		for _, h := range sec.hunks {
			hCopy := h
			records = append(records, domain.FileAccessRecord{
				AgentRole: agentRole,
				Path:      sec.path,
				Kind:      domain.FileAccessDiff,
				LineRange: &hCopy,
			})
		}
	}

	return records
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
