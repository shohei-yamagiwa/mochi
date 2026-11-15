package input

import (
	posixpath "path"
	"regexp"
	"strconv"
	"strings"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

var hunkHeaderRegex = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// ParsePatchHunks extracts 1-indexed new-file line ranges from a single file's unified diff patch.
func ParsePatchHunks(patch string) []domain.LineRange {
	hunks := make([]domain.LineRange, 0)
	for line := range strings.SplitSeq(patch, "\n") {
		m := hunkHeaderRegex.FindStringSubmatch(line)
		if len(m) == 0 {
			continue
		}
		start, err := strconv.Atoi(m[3])
		if err != nil || start <= 0 {
			continue
		}
		count := 1
		if m[4] != "" {
			if c, err := strconv.Atoi(m[4]); err == nil {
				count = c
			}
		}
		end := start
		if count > 1 {
			end = start + count - 1
		}
		hunks = append(hunks, domain.LineRange{
			StartLine: start,
			EndLine:   end,
		})
	}
	return hunks
}

// ValidDiffLines returns the set of 1-indexed new-file (RIGHT-side) line numbers that appear
// within a unified diff patch's hunks (context ' ' or added '+' lines).
// GitHub's Pull Request Review API only accepts inline comments on lines present in this set.
func ValidDiffLines(patch string) map[int]bool {
	lines := make(map[int]bool)
	if strings.TrimSpace(patch) == "" {
		return lines
	}

	inHunk := false
	newLine := 0
	oldRem := 0
	newRem := 0

	for rawLine := range strings.SplitSeq(patch, "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if m := hunkHeaderRegex.FindStringSubmatch(line); len(m) > 0 {
			start, err := strconv.Atoi(m[3])
			if err != nil {
				inHunk = false
				continue
			}
			oldRem = parseHunkCount(m[2])
			newRem = parseHunkCount(m[4])
			newLine = start
			inHunk = oldRem > 0 || newRem > 0
			continue
		}
		if !inHunk {
			continue
		}
		if strings.HasPrefix(line, "diff --git ") {
			inHunk = false
			continue
		}
		if line == `\ No newline at end of file` {
			continue
		}
		if oldRem == 0 && newRem == 0 {
			inHunk = false
			continue
		}
		if len(line) == 0 {
			// Blank context line inside a diff hunk (e.g. if trailing space was stripped).
			if oldRem > 0 && newRem > 0 {
				if newLine > 0 {
					lines[newLine] = true
					newLine++
				}
				oldRem--
				newRem--
				if oldRem == 0 && newRem == 0 {
					inHunk = false
				}
			}
			continue
		}
		switch line[0] {
		case ' ':
			if newLine > 0 {
				lines[newLine] = true
				newLine++
			}
			if oldRem > 0 {
				oldRem--
			}
			if newRem > 0 {
				newRem--
			}
		case '+':
			if newLine > 0 {
				lines[newLine] = true
				newLine++
			}
			if newRem > 0 {
				newRem--
			}
		case '-':
			if oldRem > 0 {
				oldRem--
			}
		}
		if oldRem == 0 && newRem == 0 {
			inHunk = false
		}
	}
	return lines
}

func parseHunkCount(raw string) int {
	if raw == "" {
		return 1
	}
	c, err := strconv.Atoi(raw)
	if err != nil || c < 0 {
		return 1
	}
	return c
}

// ParseUnifiedDiff parses a full git unified diff string into structured ChangedFile entries
// with per-file patches, line counts, and 1-indexed ChangedHunks.
func ParseUnifiedDiff(unifiedDiff string) []domain.ChangedFile {
	if strings.TrimSpace(unifiedDiff) == "" {
		return nil
	}

	type diffBuilder struct {
		file       domain.ChangedFile
		patchLines []string
		inHunks    bool
	}

	var files []domain.ChangedFile
	var cur *diffBuilder

	flush := func() {
		if cur == nil {
			return
		}
		cur.file.Patch = strings.Join(cur.patchLines, "\n")
		if cur.file.Status != "deleted" && cur.file.Patch != "" {
			cur.file.ChangedHunks = ParsePatchHunks(cur.file.Patch)
		}
		if cur.file.Path != "" {
			files = append(files, cur.file)
		}
		cur = nil
	}

	for rawLine := range strings.SplitSeq(unifiedDiff, "\n") {
		line := strings.TrimSuffix(rawLine, "\r")

		if strings.HasPrefix(line, "diff --git ") {
			flush()
			oldPath, newPath := parseDiffGitHeader(line)
			status := "modified"
			path := newPath
			if path == "" {
				path = oldPath
			}
			cur = &diffBuilder{
				file: domain.ChangedFile{
					Path:   path,
					Status: status,
				},
			}
			if oldPath != "" && newPath != "" && oldPath != newPath {
				cur.file.PreviousPath = oldPath
			}
			continue
		}

		if cur == nil {
			continue
		}

		if !cur.inHunks {
			switch {
			case strings.HasPrefix(line, "new file mode "):
				cur.file.Status = "added"
				continue
			case strings.HasPrefix(line, "deleted file mode "):
				cur.file.Status = "deleted"
				continue
			case strings.HasPrefix(line, "rename from "):
				cur.file.Status = "renamed"
				cur.file.PreviousPath = cleanDiffPath(strings.TrimPrefix(line, "rename from "))
				continue
			case strings.HasPrefix(line, "rename to "):
				cur.file.Status = "renamed"
				cur.file.Path = cleanDiffPath(strings.TrimPrefix(line, "rename to "))
				continue
			case strings.HasPrefix(line, "--- "):
				src := strings.TrimSpace(strings.TrimPrefix(line, "--- "))
				if src == "/dev/null" {
					cur.file.Status = "added"
				} else if cur.file.PreviousPath == "" {
					if p := stripPrefixAorB(src, "a/"); p != "" && p != cur.file.Path && cur.file.Status == "renamed" {
						cur.file.PreviousPath = p
					}
				}
				continue
			case strings.HasPrefix(line, "+++ "):
				dst := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
				if dst == "/dev/null" {
					cur.file.Status = "deleted"
				} else if p := stripPrefixAorB(dst, "b/"); p != "" {
					cur.file.Path = p
				}
				continue
			case strings.HasPrefix(line, "@@ "):
				cur.inHunks = true
				cur.patchLines = append(cur.patchLines, line)
				continue
			default:
				continue
			}
		}

		cur.patchLines = append(cur.patchLines, line)
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") {
			cur.file.Additions++
		} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- ") {
			cur.file.Deletions++
		}
	}

	flush()
	return files
}

// ParseDiffLinesByFile maps each file path in a unified diff to its valid RIGHT-side diff line numbers.
func ParseDiffLinesByFile(unifiedDiff string) map[string]map[int]bool {
	files := ParseUnifiedDiff(unifiedDiff)
	out := make(map[string]map[int]bool, len(files))
	for _, f := range files {
		if f.Path == "" || f.Status == "deleted" || f.Patch == "" {
			continue
		}
		out[f.Path] = ValidDiffLines(f.Patch)
	}
	return out
}

func parseDiffGitHeader(line string) (oldPath, newPath string) {
	rest := strings.TrimPrefix(line, "diff --git ")
	// Handle quoted paths (e.g. "a/file name.go" "b/file name.go") or standard a/... b/...
	if strings.HasPrefix(rest, `"`) {
		parts := splitQuotedPaths(rest)
		if len(parts) == 2 {
			return stripPrefixAorB(parts[0], "a/"), stripPrefixAorB(parts[1], "b/")
		}
	}
	if idx := strings.Index(rest, " b/"); idx >= 0 {
		left := strings.TrimSpace(rest[:idx])
		right := strings.TrimSpace(rest[idx+1:])
		return stripPrefixAorB(left, "a/"), stripPrefixAorB(right, "b/")
	}
	return "", ""
}

func splitQuotedPaths(s string) []string {
	var parts []string
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '"' {
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				if s[j] == '"' {
					break
				}
				j++
			}
			if j < len(s) {
				unquoted, err := strconv.Unquote(s[i : j+1])
				if err == nil {
					parts = append(parts, unquoted)
				} else {
					parts = append(parts, s[i+1:j])
				}
				i = j + 1
				continue
			}
		}
		j := i
		for j < len(s) && s[j] != ' ' {
			j++
		}
		parts = append(parts, s[i:j])
		i = j
	}
	return parts
}

func stripPrefixAorB(raw, prefix string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) && len(s) >= 2 {
		if u, err := strconv.Unquote(s); err == nil {
			s = u
		}
	}
	if s == "/dev/null" {
		return ""
	}
	s = strings.TrimPrefix(s, prefix)
	return cleanDiffPath(s)
}

func cleanDiffPath(raw string) string {
	p := strings.TrimSpace(strings.ReplaceAll(raw, `\`, "/"))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ""
	}
	cleaned := posixpath.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}
