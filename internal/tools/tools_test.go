package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/llm"
	"github.com/shohei-yamagiwa/mochi/internal/sandbox"
)

type mockSandbox struct {
	id          string
	files       map[string]string
	dirEntries  map[string][]sandbox.FileEntry
	searchHits  []sandbox.SearchMatch
	lastExecReq sandbox.ExecRequest
	execResult  *sandbox.ExecResult
	execErr     error
}

func newMockSandbox() *mockSandbox {
	return &mockSandbox{
		id:         "sb-mock-1",
		files:      make(map[string]string),
		dirEntries: make(map[string][]sandbox.FileEntry),
	}
}

func (m *mockSandbox) ID() string { return m.id }

func (m *mockSandbox) Fork(_ context.Context, name string) (sandbox.Sandbox, error) {
	clone := newMockSandbox()
	clone.id = m.id + "-fork-" + name
	for k, v := range m.files {
		clone.files[k] = v
	}
	return clone, nil
}

func (m *mockSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	m.lastExecReq = req
	if m.execErr != nil {
		return nil, m.execErr
	}
	if m.execResult != nil {
		return m.execResult, nil
	}
	return &sandbox.ExecResult{ExitCode: 0}, nil
}

func (m *mockSandbox) ReadFile(_ context.Context, path string, startLine, endLine int) (string, error) {
	data, ok := m.files[path]
	if !ok {
		return "", context.DeadlineExceeded
	}
	if startLine <= 0 && endLine <= 0 {
		return data, nil
	}
	lines := strings.Split(data, "\n")
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > len(lines) || endLine < startLine {
		return "", nil
	}
	return strings.Join(lines[startLine-1:endLine], "\n"), nil
}

func (m *mockSandbox) WriteFile(_ context.Context, path string, content []byte) error {
	m.files[path] = string(content)
	return nil
}

func (m *mockSandbox) ListDir(_ context.Context, path string) ([]sandbox.FileEntry, error) {
	return m.dirEntries[path], nil
}

func (m *mockSandbox) SearchFiles(_ context.Context, _, _ string) ([]sandbox.SearchMatch, error) {
	return m.searchHits, nil
}

func (m *mockSandbox) Destroy(_ context.Context) error { return nil }

func TestRegistry_ScopeForRoleAndDisallowedExecution(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if _, err := reg.ScopeForRole([]string{"read_file", "nonexistent_tool"}); err == nil {
		t.Fatalf("expected error for unknown tool")
	}

	scoped, err := reg.ScopeForRole([]string{"read_file", "list_dir", "submit_output", "read_file"})
	if err != nil {
		t.Fatalf("ScopeForRole: %v", err)
	}

	defs := scoped.Definitions()
	if len(defs) != 3 {
		t.Fatalf("expected 3 deduplicated definitions, got %d", len(defs))
	}
	if !scoped.HasTool("submit_output") || scoped.HasTool("write_file") {
		t.Fatalf("unexpected tool membership in scoped set")
	}

	sb := newMockSandbox()
	res, err := scoped.Execute(context.Background(), ExecutionContext{
		AgentRole: "security",
		Sandbox:   sb,
	}, llm.ToolCall{
		ID:        "tc_1",
		Name:      "write_file",
		Arguments: json.RawMessage(`{"path":"hack.txt","content":"nope"}`),
	})
	if err != nil {
		t.Fatalf("Execute disallowed tool returned unexpected error: %v", err)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Output, "not permitted") {
		t.Fatalf("expected disallowed tool rejection, got %+v", res)
	}
}

func TestFileOpsTools_AndPathTraversalProtection(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ts, err := reg.ScopeForRole([]string{"read_file", "write_file", "list_dir"})
	if err != nil {
		t.Fatalf("ScopeForRole: %v", err)
	}

	sb := newMockSandbox()
	sb.files["internal/app.go"] = "line1\nline2\nline3\nline4"
	sb.dirEntries["internal"] = []sandbox.FileEntry{
		{Name: "app.go", Path: "internal/app.go", IsDir: false, Size: 23},
	}

	execCtx := ExecutionContext{AgentRole: "correctness", Sandbox: sb}

	// read_file with line range
	res, err := ts.Execute(context.Background(), execCtx, llm.ToolCall{
		ID:        "tc1",
		Name:      "read_file",
		Arguments: json.RawMessage(`{"path":"./internal/app.go","start_line":2,"end_line":3}`),
	})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if res.ExitCode != 0 || res.Output != "line2\nline3" {
		t.Fatalf("unexpected read_file output: %+v", res)
	}
	if len(res.AccessedFiles) != 1 ||
		res.AccessedFiles[0].Kind != domain.FileAccessRead ||
		res.AccessedFiles[0].Path != "internal/app.go" ||
		res.AccessedFiles[0].LineRange == nil ||
		res.AccessedFiles[0].LineRange.StartLine != 2 ||
		res.AccessedFiles[0].LineRange.EndLine != 3 {
		t.Fatalf("unexpected read_file AccessedFiles: %+v", res.AccessedFiles)
	}

	// path traversal blocked
	trav, err := ts.Execute(context.Background(), execCtx, llm.ToolCall{
		ID:        "tc2",
		Name:      "read_file",
		Arguments: json.RawMessage(`{"path":"../../etc/passwd"}`),
	})
	if err != nil || trav.ExitCode == 0 {
		t.Fatalf("expected path traversal rejection, got res=%+v err=%v", trav, err)
	}

	// write_file
	wres, err := ts.Execute(context.Background(), execCtx, llm.ToolCall{
		ID:        "tc3",
		Name:      "write_file",
		Arguments: json.RawMessage(`{"path":"internal/app_test.go","content":"package main"}`),
	})
	if err != nil || wres.ExitCode != 0 {
		t.Fatalf("write_file failed: res=%+v err=%v", wres, err)
	}
	if sb.files["internal/app_test.go"] != "package main" {
		t.Fatalf("file not written to sandbox")
	}
	if len(wres.AccessedFiles) != 1 || wres.AccessedFiles[0].Kind != domain.FileAccessWrite {
		t.Fatalf("unexpected write AccessedFiles: %+v", wres.AccessedFiles)
	}

	// list_dir
	lres, err := ts.Execute(context.Background(), execCtx, llm.ToolCall{
		ID:        "tc4",
		Name:      "list_dir",
		Arguments: json.RawMessage(`{"path":"internal"}`),
	})
	if err != nil || lres.ExitCode != 0 || !strings.Contains(lres.Output, "app.go") {
		t.Fatalf("list_dir failed: res=%+v err=%v", lres, err)
	}
	if len(lres.AccessedFiles) != 1 || lres.AccessedFiles[0].Kind != domain.FileAccessList {
		t.Fatalf("unexpected list AccessedFiles: %+v", lres.AccessedFiles)
	}
}

func TestSearchCodeAndGitDiffTools(t *testing.T) {
	reg, _ := NewRegistry()
	ts, _ := reg.ScopeForRole([]string{"search_code", "git_diff"})

	sb := newMockSandbox()
	sb.searchHits = []sandbox.SearchMatch{
		{File: "internal/auth.go", LineNumber: 42, LineText: "token := req.Header.Get(\"Authorization\")"},
	}
	sb.execResult = &sandbox.ExecResult{
		ExitCode: 0,
		Stdout:   "diff --git a/internal/auth.go b/internal/auth.go\n--- a/internal/auth.go\n+++ b/internal/auth.go\n@@ -10,1 +10,2 @@\n+check()\n",
	}

	execCtx := ExecutionContext{
		AgentRole:      "security",
		Sandbox:        sb,
		BaseRef:        "abc111",
		HeadRef:        "def222",
		CommandTimeout: 10 * time.Second,
	}

	sres, err := ts.Execute(context.Background(), execCtx, jsonCall("search_code", `{"pattern":"Authorization"}`))
	if err != nil || sres.ExitCode != 0 || !strings.Contains(sres.Output, "internal/auth.go:42") {
		t.Fatalf("search_code failed: res=%+v err=%v", sres, err)
	}
	if len(sres.AccessedFiles) != 1 || sres.AccessedFiles[0].Kind != domain.FileAccessSearch || sres.AccessedFiles[0].LineRange.StartLine != 42 {
		t.Fatalf("unexpected search AccessedFiles: %+v", sres.AccessedFiles)
	}

	dres, err := ts.Execute(context.Background(), execCtx, jsonCall("git_diff", `{}`))
	if err != nil || dres.ExitCode != 0 {
		t.Fatalf("git_diff failed: res=%+v err=%v", dres, err)
	}
	if !strings.Contains(sb.lastExecReq.Command, "'abc111...def222'") {
		t.Fatalf("unexpected git diff command: %q", sb.lastExecReq.Command)
	}
	if len(dres.AccessedFiles) != 1 || dres.AccessedFiles[0].Kind != domain.FileAccessDiff || dres.AccessedFiles[0].Path != "internal/auth.go" {
		t.Fatalf("unexpected git_diff AccessedFiles: %+v", dres.AccessedFiles)
	}
}

func TestExecCommand_TruncationAndTimeoutClamp(t *testing.T) {
	reg, _ := NewRegistry()
	ts, _ := reg.ScopeForRole([]string{"exec_command"})

	sb := newMockSandbox()
	huge := strings.Repeat("A", 20*1024) + strings.Repeat("Z", 25*1024)
	sb.execResult = &sandbox.ExecResult{
		ExitCode: 0,
		Stdout:   huge,
	}

	res, err := ts.Execute(context.Background(), ExecutionContext{
		AgentRole:      "test-runner",
		Sandbox:        sb,
		CommandTimeout: 30 * time.Second,
	}, jsonCall("exec_command", `{"command":"go test ./...","timeout_seconds":120}`))
	if err != nil {
		t.Fatalf("exec_command: %v", err)
	}
	if sb.lastExecReq.Timeout != 30*time.Second {
		t.Fatalf("expected timeout clamped to 30s, got %v", sb.lastExecReq.Timeout)
	}
	if len(res.Output) > defaultMaxOutputBytes {
		t.Fatalf("expected output <= %d bytes, got %d", defaultMaxOutputBytes, len(res.Output))
	}
	if !strings.Contains(res.Output, "[truncated") || !strings.Contains(res.Output, "AAAA") || !strings.Contains(res.Output, "ZZZZ") {
		t.Fatalf("expected head+tail truncation markers & both ends preserved")
	}
}

func TestTruncateHeadTail_PreservesUTF8Boundaries(t *testing.T) {
	// Mix 3-byte Japanese runes and 4-byte emoji so byte offsets land mid-rune.
	text := strings.Repeat("日本語テスト🚀", 40)
	for _, limit := range []int{20, 48, 99, 100, 101, 200} {
		out := truncateHeadTail(text, limit)
		if len(out) > limit {
			t.Fatalf("limit=%d exceeded: len=%d", limit, len(out))
		}
		if !utf8.ValidString(out) {
			t.Fatalf("limit=%d produced invalid UTF-8: %q", limit, out)
		}
	}
}

func TestSubmitOutputTool_ValidationAndCiteRecords(t *testing.T) {
	reg, _ := NewRegistry()
	ts, _ := reg.ScopeForRole([]string{"submit_output"})
	execCtx := ExecutionContext{AgentRole: "correctness"}

	// Invalid severity & confidence should return ExitCode 1 without terminating
	badRes, err := ts.Execute(context.Background(), execCtx, jsonCall("submit_output", `{
		"summary": "bad",
		"findings": [{
			"file_path": "a.go",
			"severity": "extreme",
			"category": "correctness",
			"title": "bug",
			"description": "desc",
			"confidence": 1.5
		}]
	}`))
	if err != nil {
		t.Fatalf("submit_output invalid payload err: %v", err)
	}
	if badRes.ExitCode == 0 || badRes.IsFinalOutput {
		t.Fatalf("expected schema rejection, got %+v", badRes)
	}

	// Valid payload
	goodRes, err := ts.Execute(context.Background(), execCtx, jsonCall("submit_output", `{
		"summary": "Found 1 bug",
		"findings": [{
			"file_path": "pkg/foo.go",
			"start_line": 12,
			"end_line": 18,
			"severity": "high",
			"category": "correctness",
			"title": "Nil pointer dereference",
			"description": "Missing nil check before dereferencing user",
			"confidence": 0.92
		}]
	}`))
	if err != nil {
		t.Fatalf("submit_output valid payload err: %v", err)
	}
	if goodRes.ExitCode != 0 || !goodRes.IsFinalOutput || len(goodRes.SubmittedOutput) == 0 {
		t.Fatalf("expected accepted final output, got %+v", goodRes)
	}
	if len(goodRes.AccessedFiles) != 1 ||
		goodRes.AccessedFiles[0].Kind != domain.FileAccessCite ||
		goodRes.AccessedFiles[0].Path != "pkg/foo.go" ||
		goodRes.AccessedFiles[0].LineRange.StartLine != 12 ||
		goodRes.AccessedFiles[0].LineRange.EndLine != 18 {
		t.Fatalf("unexpected cite AccessedFiles: %+v", goodRes.AccessedFiles)
	}
}

func TestReadFileAndGitDiff_TruncatedOutputOnlyRecordsSurvivingAccess(t *testing.T) {
	ctx := context.Background()
	sb := newMockSandbox()

	// 1. Truncated read_file: 1000 lines (~50 KB), so middle lines are dropped by 32 KB truncation.
	var lines strings.Builder
	for i := 1; i <= 1000; i++ {
		lines.WriteString(strings.Repeat("x", 50) + "\n")
	}
	sb.files["pkg/big.go"] = lines.String()

	rfTool := NewReadFileTool()
	rfRes, err := rfTool.Execute(ctx, ExecutionContext{AgentRole: "correctness", Sandbox: sb}, json.RawMessage(`{"path":"pkg/big.go"}`))
	if err != nil || rfRes.ExitCode != 0 {
		t.Fatalf("read_file err=%v res=%+v", err, rfRes)
	}
	if len(rfRes.AccessedFiles) != 2 {
		t.Fatalf("len(rfRes.AccessedFiles) = %d, want 2 (head and tail ranges)", len(rfRes.AccessedFiles))
	}
	headRange := rfRes.AccessedFiles[0].LineRange
	tailRange := rfRes.AccessedFiles[1].LineRange
	if headRange == nil || tailRange == nil || headRange.EndLine >= tailRange.StartLine {
		t.Fatalf("expected disjoint head (%+v) and tail (%+v) read ranges on truncated file", headRange, tailRange)
	}

	// 2. Truncated git_diff: middle file (pkg/middle.go) is completely in the dropped middle section.
	var diffBuilder strings.Builder
	diffBuilder.WriteString("diff --git a/pkg/first.go b/pkg/first.go\n--- a/pkg/first.go\n+++ b/pkg/first.go\n@@ -1,2 +1,5 @@\n+line\n")
	diffBuilder.WriteString("diff --git a/pkg/pad_head.go b/pkg/pad_head.go\n--- a/pkg/pad_head.go\n+++ b/pkg/pad_head.go\n@@ -1,2 +1,5 @@\n")
	diffBuilder.WriteString(strings.Repeat("+padding_head\n", 1400))
	diffBuilder.WriteString("diff --git a/pkg/middle_dropped.go b/pkg/middle_dropped.go\n--- a/pkg/middle_dropped.go\n+++ b/pkg/middle_dropped.go\n@@ -1,2 +1,5 @@\n+dropped\n")
	diffBuilder.WriteString(strings.Repeat("+padding_tail\n", 1400))
	diffBuilder.WriteString("diff --git a/pkg/last.go b/pkg/last.go\n--- a/pkg/last.go\n+++ b/pkg/last.go\n@@ -1,2 +1,5 @@\n+last\n")

	sb.execResult = &sandbox.ExecResult{
		ExitCode: 0,
		Stdout:   diffBuilder.String(),
	}

	gdTool := NewGitDiffTool()
	gdRes, err := gdTool.Execute(ctx, ExecutionContext{AgentRole: "security", Sandbox: sb}, json.RawMessage(`{}`))
	if err != nil || gdRes.ExitCode != 0 {
		t.Fatalf("git_diff err=%v res=%+v", err, gdRes)
	}
	for _, rec := range gdRes.AccessedFiles {
		if rec.Path == "pkg/middle_dropped.go" {
			t.Fatalf("pkg/middle_dropped.go was dropped by truncation and must not appear in AccessedFiles: %+v", gdRes.AccessedFiles)
		}
	}
}

func jsonCall(name, args string) llm.ToolCall {
	return llm.ToolCall{
		ID:        "call_" + name,
		Name:      name,
		Arguments: json.RawMessage(args),
	}
}

type localExecSandbox struct {
	*mockSandbox
	dir string
}

func (l *localExecSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	l.lastExecReq = req
	cmd := exec.Command("sh", "-c", req.Command)
	cmd.Dir = l.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return &sandbox.ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func TestGitDiff_ThreeDotExcludesBaseBranchAdvance(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found")
	}

	dir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Mochi Test",
			"GIT_AUTHOR_EMAIL=mochi@example.com",
			"GIT_COMMITTER_NAME=Mochi Test",
			"GIT_COMMITTER_EMAIL=mochi@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "base.txt")
	runGit("commit", "-m", "initial")

	// Branch PR off initial commit
	runGit("checkout", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "pr.txt"), []byte("pr-change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "pr.txt")
	runGit("commit", "-m", "pr change")
	headSHA := runGit("rev-parse", "HEAD")

	// Advance main after branching (e.g. merged another PR into main)
	runGit("checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "base_later.txt"), []byte("from another PR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "base_later.txt")
	runGit("commit", "-m", "advance main")
	baseSHA := runGit("rev-parse", "HEAD")

	runGit("checkout", headSHA)

	reg, _ := NewRegistry()
	ts, _ := reg.ScopeForRole([]string{"git_diff"})
	sb := &localExecSandbox{mockSandbox: newMockSandbox(), dir: dir}

	res, err := ts.Execute(context.Background(), ExecutionContext{
		AgentRole: "correctness",
		Sandbox:   sb,
		BaseRef:   baseSHA,
		HeadRef:   headSHA,
	}, jsonCall("git_diff", `{}`))
	if err != nil {
		t.Fatalf("git_diff failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("git_diff exit=%d output=%s", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "pr.txt") || !strings.Contains(res.Output, "+pr-change") {
		t.Fatalf("expected PR change in diff output, got:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "base_later.txt") || strings.Contains(res.Output, "-from another PR") {
		t.Fatalf("three-dot diff must NOT show base branch advance as reverted, got:\n%s", res.Output)
	}
}

func TestReadFile_ClampsTailRangeToEOF(t *testing.T) {
	reg, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ts, err := reg.ScopeForRole([]string{"read_file"})
	if err != nil {
		t.Fatalf("ScopeForRole: %v", err)
	}

	var b strings.Builder
	for i := 1; i <= 2000; i++ {
		if i > 1 {
			b.WriteByte('\n')
		}
		b.WriteString("// line padding to trigger 32KB head+tail truncation in read_file")
	}

	sb := newMockSandbox()
	sb.files["big.go"] = b.String()

	res, err := ts.Execute(context.Background(), ExecutionContext{
		AgentRole: "correctness",
		Sandbox:   sb,
	}, jsonCall("read_file", `{"path":"big.go","start_line":1,"end_line":99999}`))
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("read_file exit=%d output=%s", res.ExitCode, res.Output)
	}
	if len(res.AccessedFiles) != 2 {
		t.Fatalf("expected head+tail FileAccessRecords (2), got %+v", res.AccessedFiles)
	}
	headRec := res.AccessedFiles[0]
	tailRec := res.AccessedFiles[1]
	if headRec.LineRange == nil || headRec.LineRange.StartLine != 1 {
		t.Fatalf("unexpected head range: %+v", headRec.LineRange)
	}
	if tailRec.LineRange == nil || tailRec.LineRange.EndLine != 2000 {
		t.Fatalf("expected tail EndLine clamped to 2000, got %+v", tailRec.LineRange)
	}
	if tailRec.LineRange.StartLine > 1990 || tailRec.LineRange.EndLine < 1995 {
		t.Fatalf("expected tail range to cover hunk 1990-1995, got %+v", tailRec.LineRange)
	}
}

