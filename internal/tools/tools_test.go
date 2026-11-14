package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	if !strings.Contains(sb.lastExecReq.Command, "'abc111..def222'") {
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

func jsonCall(name, args string) llm.ToolCall {
	return llm.ToolCall{
		ID:        "call_" + name,
		Name:      name,
		Arguments: json.RawMessage(args),
	}
}
