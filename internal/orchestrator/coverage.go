package orchestrator

import (
	posixpath "path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
)

var hunkHeaderRegex = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ComputeCoverageReport deterministically measures the gap between a changeset's modified
// files/hunks and the repository files/line-ranges actually inspected by specialist agents.
func ComputeCoverageReport(
	runID string,
	changeset *domain.ChangesetSpec,
	toolExecs []*domain.ToolExecRecord,
) *domain.CoverageReport {
	report := &domain.CoverageReport{
		RunID:             runID,
		FileCoverageRatio: 1.0,
		HunkCoverageRatio: 1.0,
		UnreviewedFiles:   []string{},
		FileDetails:       []domain.FileCoverageDetail{},
		ContextFilesRead:  []string{},
		CreatedAt:         time.Now().UTC(),
	}
	if changeset == nil {
		return report
	}

	report.PRNumber = changeset.PRNumber
	report.BaseSHA = changeset.BaseSHA
	report.HeadSHA = changeset.HeadSHA

	unifiedHunksByFile := ParseUnifiedDiffHunks(changeset.UnifiedDiff)

	type fileTracker struct {
		path              string
		prevPath          string
		status            string
		hunks             []domain.LineRange
		hunkCovered       []bool
		readRangesByAgent map[string][]domain.LineRange
		inspectedByAgents []string
		findingsCount     int
	}

	trackers := make([]*fileTracker, 0, len(changeset.ChangedFiles))
	byPath := make(map[string]*fileTracker, len(changeset.ChangedFiles)*2)

	for _, cf := range changeset.ChangedFiles {
		cleanPath := normalizeCoveragePath(cf.Path)
		if cleanPath == "" {
			continue
		}
		cleanPrev := normalizeCoveragePath(cf.PreviousPath)

		hunks := normalizeLineRanges(cf.ChangedHunks)
		if len(hunks) == 0 && cf.Status != "deleted" {
			if strings.TrimSpace(cf.Patch) != "" {
				hunks = ParsePatchHunks(cf.Patch)
			} else if fromUnified, ok := unifiedHunksByFile[cleanPath]; ok {
				hunks = normalizeLineRanges(fromUnified)
			}
		}

		ft := &fileTracker{
			path:              cleanPath,
			prevPath:          cleanPrev,
			status:            cf.Status,
			hunks:             hunks,
			hunkCovered:       make([]bool, len(hunks)),
			readRangesByAgent: make(map[string][]domain.LineRange),
			inspectedByAgents: []string{},
		}
		trackers = append(trackers, ft)
		byPath[cleanPath] = ft
		if cleanPrev != "" {
			byPath[cleanPrev] = ft
		}
	}

	contextFilesSet := make(map[string]struct{})
	var contextFiles []string

	markAllHunksCovered := func(ft *fileTracker) {
		for i := range ft.hunkCovered {
			ft.hunkCovered[i] = true
		}
	}

	markOverlappingHunksCovered := func(ft *fileTracker, lr domain.LineRange) {
		norm := normalizeLineRange(lr)
		for i, h := range ft.hunks {
			if rangesOverlap(h, norm) {
				ft.hunkCovered[i] = true
			}
		}
	}

	addAgent := func(ft *fileTracker, role string) {
		role = strings.TrimSpace(role)
		if role == "" {
			return
		}
		if !slices.Contains(ft.inspectedByAgents, role) {
			ft.inspectedByAgents = append(ft.inspectedByAgents, role)
		}
	}

	addReadRange := func(ft *fileTracker, role string, lr domain.LineRange) {
		role = strings.TrimSpace(role)
		if role == "" {
			return
		}
		norm := normalizeLineRange(lr)
		ft.readRangesByAgent[role] = mergeLineRanges(append(ft.readRangesByAgent[role], norm))
	}

	for _, te := range toolExecs {
		if te == nil || te.ExitCode != 0 {
			continue
		}
		for _, rec := range te.AccessedFiles {
			cleanRecPath := normalizeCoveragePath(rec.Path)
			if cleanRecPath == "" || cleanRecPath == "." {
				continue
			}

			var matched []*fileTracker
			if ft, ok := byPath[cleanRecPath]; ok {
				matched = append(matched, ft)
			} else if rec.Kind == domain.FileAccessDiff {
				prefix := cleanRecPath + "/"
				for _, ft := range trackers {
					if strings.HasPrefix(ft.path, prefix) || (ft.prevPath != "" && strings.HasPrefix(ft.prevPath, prefix)) {
						matched = append(matched, ft)
					}
				}
			}

			if len(matched) == 0 {
				if rec.Kind == domain.FileAccessRead {
					if _, seen := contextFilesSet[cleanRecPath]; !seen {
						contextFilesSet[cleanRecPath] = struct{}{}
						contextFiles = append(contextFiles, cleanRecPath)
					}
				}
				continue
			}

			for _, ft := range matched {
				switch rec.Kind {
				case domain.FileAccessRead:
					addAgent(ft, rec.AgentRole)
					if rec.LineRange != nil {
						addReadRange(ft, rec.AgentRole, *rec.LineRange)
						markOverlappingHunksCovered(ft, *rec.LineRange)
					} else {
						markAllHunksCovered(ft)
					}
				case domain.FileAccessDiff:
					addAgent(ft, rec.AgentRole)
					if rec.LineRange != nil {
						markOverlappingHunksCovered(ft, *rec.LineRange)
					} else {
						markAllHunksCovered(ft)
					}
				case domain.FileAccessSearch:
					// Record search hits in ReadRangesByAgent for visibility, but do not mark
					// entire hunks covered or the file inspected from a single-line grep match.
					if rec.LineRange != nil {
						addReadRange(ft, rec.AgentRole, *rec.LineRange)
					}
				case domain.FileAccessCite:
					addAgent(ft, rec.AgentRole)
					ft.findingsCount++
					if rec.LineRange != nil {
						markOverlappingHunksCovered(ft, *rec.LineRange)
					}
				}
			}
		}
	}

	report.TotalChangedFiles = len(trackers)
	report.FileDetails = make([]domain.FileCoverageDetail, 0, len(trackers))

	for _, ft := range trackers {
		uncovered := make([]domain.LineRange, 0)
		for i, h := range ft.hunks {
			report.TotalChangedHunks++
			if ft.hunkCovered[i] {
				report.ReviewedChangedHunks++
			} else {
				uncovered = append(uncovered, h)
			}
		}

		if len(ft.inspectedByAgents) > 0 {
			report.ReviewedChangedFiles++
		} else {
			report.UnreviewedFiles = append(report.UnreviewedFiles, ft.path)
		}

		report.FileDetails = append(report.FileDetails, domain.FileCoverageDetail{
			Path:              ft.path,
			ChangeStatus:      ft.status,
			ChangedHunks:      ft.hunks,
			ReadRangesByAgent: ft.readRangesByAgent,
			InspectedByAgents: ft.inspectedByAgents,
			UncoveredHunks:    uncovered,
			FindingsCount:     ft.findingsCount,
		})
	}

	if report.TotalChangedFiles > 0 {
		report.FileCoverageRatio = float64(report.ReviewedChangedFiles) / float64(report.TotalChangedFiles)
	}
	if report.TotalChangedHunks > 0 {
		report.HunkCoverageRatio = float64(report.ReviewedChangedHunks) / float64(report.TotalChangedHunks)
	} else if report.TotalChangedFiles > 0 {
		report.HunkCoverageRatio = report.FileCoverageRatio
	}

	if len(contextFiles) > 0 {
		slices.Sort(contextFiles)
		report.ContextFilesRead = contextFiles
	}

	return report
}

// ParsePatchHunks extracts 1-indexed new-file line ranges from a single file's unified diff patch.
func ParsePatchHunks(patch string) []domain.LineRange {
	hunks := make([]domain.LineRange, 0)
	for line := range strings.SplitSeq(patch, "\n") {
		m := hunkHeaderRegex.FindStringSubmatch(line)
		if len(m) == 0 {
			continue
		}
		start, err := strconv.Atoi(m[1])
		if err != nil || start <= 0 {
			continue
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
		hunks = append(hunks, domain.LineRange{
			StartLine: start,
			EndLine:   end,
		})
	}
	return hunks
}

// ParseUnifiedDiffHunks parses a multi-file unified diff and maps each repository-relative file path
// to its modified new-file line ranges.
func ParseUnifiedDiffHunks(unifiedDiff string) map[string][]domain.LineRange {
	out := make(map[string][]domain.LineRange)
	if strings.TrimSpace(unifiedDiff) == "" {
		return out
	}

	var currentFile string
	var currentPatch strings.Builder

	flush := func() {
		if currentFile != "" && currentPatch.Len() > 0 {
			hunks := ParsePatchHunks(currentPatch.String())
			if len(hunks) > 0 {
				out[currentFile] = append(out[currentFile], hunks...)
			}
		}
		currentPatch.Reset()
	}

	for line := range strings.SplitSeq(unifiedDiff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			currentFile = ""
			rest := strings.TrimPrefix(line, "diff --git ")
			if idx := strings.Index(rest, " b/"); idx >= 0 {
				p := normalizeCoveragePath(rest[idx+3:])
				if p != "" && p != "dev/null" {
					currentFile = p
				}
			}
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			target := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if target == "/dev/null" {
				currentFile = ""
				continue
			}
			if strings.HasPrefix(target, "b/") {
				if p := normalizeCoveragePath(strings.TrimPrefix(target, "b/")); p != "" {
					currentFile = p
				}
			}
			continue
		}
		if currentFile != "" && strings.HasPrefix(line, "@@ ") {
			currentPatch.WriteString(line)
			currentPatch.WriteByte('\n')
		}
	}
	flush()
	return out
}

func normalizeCoveragePath(raw string) string {
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

func normalizeLineRange(lr domain.LineRange) domain.LineRange {
	start := lr.StartLine
	if start <= 0 {
		start = 1
	}
	end := lr.EndLine
	if end < start {
		end = start
	}
	return domain.LineRange{StartLine: start, EndLine: end}
}

func normalizeLineRanges(ranges []domain.LineRange) []domain.LineRange {
	if len(ranges) == 0 {
		return []domain.LineRange{}
	}
	out := make([]domain.LineRange, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, normalizeLineRange(r))
	}
	return out
}

func mergeLineRanges(ranges []domain.LineRange) []domain.LineRange {
	if len(ranges) == 0 {
		return []domain.LineRange{}
	}
	sorted := make([]domain.LineRange, len(ranges))
	for i, r := range ranges {
		sorted[i] = normalizeLineRange(r)
	}
	slices.SortFunc(sorted, func(a, b domain.LineRange) int {
		if a.StartLine != b.StartLine {
			return a.StartLine - b.StartLine
		}
		return a.EndLine - b.EndLine
	})

	merged := make([]domain.LineRange, 0, len(sorted))
	cur := sorted[0]
	for _, next := range sorted[1:] {
		if next.StartLine <= cur.EndLine+1 {
			if next.EndLine > cur.EndLine {
				cur.EndLine = next.EndLine
			}
		} else {
			merged = append(merged, cur)
			cur = next
		}
	}
	merged = append(merged, cur)
	return merged
}

func rangesOverlap(a, b domain.LineRange) bool {
	return a.StartLine <= b.EndLine && b.StartLine <= a.EndLine
}
