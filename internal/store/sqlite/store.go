package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/shohei-yamagiwa/mochi/internal/domain"
	"github.com/shohei-yamagiwa/mochi/internal/store"
)

// sqliteTimeLayout uses fixed-width 9-digit fractional seconds in UTC so TEXT ORDER BY matches chronological order.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

// SQLiteStore implements store.Store backed by CGO-free embedded SQLite (modernc.org/sqlite).
type SQLiteStore struct {
	db *sql.DB
}

// Compile-time check that SQLiteStore satisfies store.Store.
var _ store.Store = (*SQLiteStore)(nil)

// Open opens (or creates) a SQLite database at path, applies connection-persistent DSN pragmas and
// single-writer pool tuning, and runs schema migrations.
// Use ":memory:" for an ephemeral in-memory database (primarily for tests).
func Open(ctx context.Context, path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite database path must not be empty")
	}

	isMemory := path == ":memory:" || strings.HasPrefix(path, "file::memory:")
	if !isMemory && !strings.HasPrefix(path, "file:") {
		dir := filepath.Dir(path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create sqlite directory %q: %w", dir, err)
			}
		}
	}

	dsn, err := buildSQLiteDSN(path, isMemory)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	// Single-writer pool tuning avoids SQLITE_BUSY contention with embedded SQLite.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}

	if err := runMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

func buildSQLiteDSN(path string, isMemory bool) (string, error) {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if !isMemory {
		q.Add("_pragma", "journal_mode(WAL)")
	}

	if strings.HasPrefix(path, "file:") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		return path + sep + q.Encode(), nil
	}
	if path == ":memory:" {
		return "file::memory:?" + q.Encode(), nil
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve sqlite path %q: %w", path, err)
	}
	slashPath := filepath.ToSlash(absPath)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	u := url.URL{
		Scheme:   "file",
		Path:     slashPath,
		RawQuery: q.Encode(),
	}
	return u.String(), nil
}

// Close closes the underlying database connection pool.
func (s *SQLiteStore) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) CreateRun(ctx context.Context, run *domain.RunRecord) error {
	if run == nil {
		return fmt.Errorf("run must not be nil")
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_runs (
			run_id, workflow_name, trigger_type, dedup_key, status,
			resolved_config_yaml, final_artifact_json, error_message, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		run.RunID,
		run.WorkflowName,
		run.TriggerType,
		run.DedupKey,
		string(run.Status),
		run.ResolvedConfigYAML,
		nullableJSON(run.FinalArtifactJSON),
		run.ErrorMessage,
		formatTime(run.StartedAt),
		formatNullableTime(run.CompletedAt),
	)
	if err != nil {
		return fmt.Errorf("insert workflow_run %q: %w", run.RunID, err)
	}
	return nil
}

func (s *SQLiteStore) UpdateRunStatus(ctx context.Context, runID string, status domain.RunStatus, errMsg string, completedAt *time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_runs
		SET status = ?, error_message = ?, completed_at = COALESCE(?, completed_at)
		WHERE run_id = ?
	`, string(status), errMsg, formatNullableTime(completedAt), runID)
	if err != nil {
		return fmt.Errorf("update workflow_run %q status: %w", runID, err)
	}
	return requireOneRowAffected(res)
}

func (s *SQLiteStore) UpdateRunResolvedConfig(ctx context.Context, runID string, resolvedConfigYAML string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_runs
		SET resolved_config_yaml = ?
		WHERE run_id = ?
	`, resolvedConfigYAML, runID)
	if err != nil {
		return fmt.Errorf("update workflow_run %q resolved_config_yaml: %w", runID, err)
	}
	return requireOneRowAffected(res)
}

func (s *SQLiteStore) CompleteRun(ctx context.Context, runID string, status domain.RunStatus, finalArtifact json.RawMessage, completedAt time.Time) error {
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_runs
		SET status = ?, final_artifact_json = ?, completed_at = ?
		WHERE run_id = ?
	`, string(status), nullableJSON(finalArtifact), formatTime(completedAt), runID)
	if err != nil {
		return fmt.Errorf("complete workflow_run %q: %w", runID, err)
	}
	return requireOneRowAffected(res)
}

func (s *SQLiteStore) GetRun(ctx context.Context, runID string) (*domain.RunRecord, error) {
	return getRun(ctx, s.db, runID)
}

func (s *SQLiteStore) ListRuns(ctx context.Context, filter store.RunFilter) ([]*domain.RunRecord, error) {
	var clauses []string
	var args []any

	if filter.WorkflowName != "" {
		clauses = append(clauses, "workflow_name = ?")
		args = append(args, filter.WorkflowName)
	}
	if filter.DedupKey != "" {
		clauses = append(clauses, "dedup_key = ?")
		args = append(args, filter.DedupKey)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(filter.Status))
	}

	query := `
		SELECT run_id, workflow_name, trigger_type, dedup_key, status,
		       resolved_config_yaml, final_artifact_json, error_message, started_at, completed_at
		FROM workflow_runs
	`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY started_at DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow_runs: %w", err)
	}
	defer rows.Close()

	var out []*domain.RunRecord
	for rows.Next() {
		rec, err := scanRunRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workflow_run: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow_runs: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) SaveChangesetSnapshot(ctx context.Context, runID string, cs *domain.ChangesetSpec) error {
	if cs == nil {
		return fmt.Errorf("changeset must not be nil")
	}
	changedFiles := cs.ChangedFiles
	if changedFiles == nil {
		changedFiles = []domain.ChangedFile{}
	}
	changedFilesJSON, err := json.Marshal(changedFiles)
	if err != nil {
		return fmt.Errorf("marshal changed_files: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO changeset_snapshots (
			run_id, provider, repo_owner, repo_name, pr_number, pr_url,
			pr_title, pr_description, pr_author, trigger_event, installation_id,
			base_branch, base_sha, head_branch, head_sha, unified_diff, changed_files_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			provider = excluded.provider,
			repo_owner = excluded.repo_owner,
			repo_name = excluded.repo_name,
			pr_number = excluded.pr_number,
			pr_url = excluded.pr_url,
			pr_title = excluded.pr_title,
			pr_description = excluded.pr_description,
			pr_author = excluded.pr_author,
			trigger_event = excluded.trigger_event,
			installation_id = excluded.installation_id,
			base_branch = excluded.base_branch,
			base_sha = excluded.base_sha,
			head_branch = excluded.head_branch,
			head_sha = excluded.head_sha,
			unified_diff = excluded.unified_diff,
			changed_files_json = excluded.changed_files_json
	`,
		runID,
		cs.Provider,
		cs.RepoOwner,
		cs.RepoName,
		cs.PRNumber,
		cs.PRURL,
		cs.Title,
		cs.Description,
		cs.Author,
		cs.TriggerEvent,
		cs.InstallationID,
		cs.BaseBranch,
		cs.BaseSHA,
		cs.HeadBranch,
		cs.HeadSHA,
		cs.UnifiedDiff,
		string(changedFilesJSON),
	)
	if err != nil {
		return fmt.Errorf("save changeset_snapshot for %q: %w", runID, err)
	}
	return nil
}

func (s *SQLiteStore) GetChangesetSnapshot(ctx context.Context, runID string) (*domain.ChangesetSpec, error) {
	return getChangesetSnapshot(ctx, s.db, runID)
}

func (s *SQLiteStore) CreateAgentRun(ctx context.Context, ar *domain.AgentRunRecord) error {
	if ar == nil {
		return fmt.Errorf("agent run must not be nil")
	}
	if ar.StartedAt.IsZero() {
		ar.StartedAt = time.Now().UTC()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_runs (
			agent_run_id, run_id, role, provider, model, sandbox_id,
			rendered_system_prompt, rendered_user_prompt, status, turns_used,
			input_tokens, output_tokens, cached_tokens, submitted_output_json,
			error_message, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		ar.AgentRunID,
		ar.RunID,
		ar.Role,
		ar.Provider,
		ar.Model,
		ar.SandboxID,
		ar.RenderedSystemPrompt,
		ar.RenderedUserPrompt,
		string(ar.Status),
		ar.TurnsUsed,
		ar.InputTokens,
		ar.OutputTokens,
		ar.CachedTokens,
		nullableJSON(ar.SubmittedOutputJSON),
		ar.ErrorMessage,
		formatTime(ar.StartedAt),
		formatNullableTime(ar.CompletedAt),
	)
	if err != nil {
		return fmt.Errorf("insert agent_run %q: %w", ar.AgentRunID, err)
	}
	return nil
}

func (s *SQLiteStore) UpdateAgentRun(ctx context.Context, ar *domain.AgentRunRecord) error {
	if ar == nil {
		return fmt.Errorf("agent run must not be nil")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE agent_runs
		SET sandbox_id = ?,
		    status = ?,
		    turns_used = ?,
		    input_tokens = ?,
		    output_tokens = ?,
		    cached_tokens = ?,
		    submitted_output_json = ?,
		    error_message = ?,
		    completed_at = COALESCE(?, completed_at)
		WHERE agent_run_id = ?
	`,
		ar.SandboxID,
		string(ar.Status),
		ar.TurnsUsed,
		ar.InputTokens,
		ar.OutputTokens,
		ar.CachedTokens,
		nullableJSON(ar.SubmittedOutputJSON),
		ar.ErrorMessage,
		formatNullableTime(ar.CompletedAt),
		ar.AgentRunID,
	)
	if err != nil {
		return fmt.Errorf("update agent_run %q: %w", ar.AgentRunID, err)
	}
	return requireOneRowAffected(res)
}

func (s *SQLiteStore) ListAgentRuns(ctx context.Context, runID string) ([]*domain.AgentRunRecord, error) {
	return listAgentRuns(ctx, s.db, runID)
}

func (s *SQLiteStore) SaveLLMTurn(ctx context.Context, turn *domain.LLMTurnRecord) error {
	if turn == nil {
		return fmt.Errorf("llm turn must not be nil")
	}
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now().UTC()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO llm_turns (
			turn_id, agent_run_id, turn_index, request_messages_json, response_message_json,
			thinking_trace, tool_calls_json, input_tokens, output_tokens, cached_tokens,
			latency_ms, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		turn.TurnID,
		turn.AgentRunID,
		turn.TurnIndex,
		defaultJSON(turn.RequestMessagesJSON, "[]"),
		defaultJSON(turn.ResponseMessageJSON, "{}"),
		turn.ThinkingTrace,
		nullableJSON(turn.ToolCallsJSON),
		turn.InputTokens,
		turn.OutputTokens,
		turn.CachedTokens,
		turn.LatencyMS,
		formatTime(turn.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert llm_turn %q: %w", turn.TurnID, err)
	}
	return nil
}

func (s *SQLiteStore) ListLLMTurns(ctx context.Context, agentRunID string) ([]*domain.LLMTurnRecord, error) {
	return listLLMTurns(ctx, s.db, agentRunID)
}

func (s *SQLiteStore) SaveToolExecution(ctx context.Context, exec *domain.ToolExecRecord) error {
	if exec == nil {
		return fmt.Errorf("tool execution must not be nil")
	}
	if exec.CreatedAt.IsZero() {
		exec.CreatedAt = time.Now().UTC()
	}

	accessed := exec.AccessedFiles
	if accessed == nil {
		accessed = []domain.FileAccessRecord{}
	}
	accessedJSON, err := json.Marshal(accessed)
	if err != nil {
		return fmt.Errorf("marshal accessed_files: %w", err)
	}

	var turnID sql.NullString
	if exec.TurnID != "" {
		turnID = sql.NullString{String: exec.TurnID, Valid: true}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tool_executions (
			tool_exec_id, agent_run_id, turn_id, tool_call_id, tool_name,
			arguments_json, output_text, exit_code, accessed_files_json, duration_ms, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		exec.ToolExecID,
		exec.AgentRunID,
		turnID,
		exec.ToolCallID,
		exec.ToolName,
		defaultJSON(exec.ArgumentsJSON, "{}"),
		exec.OutputText,
		exec.ExitCode,
		string(accessedJSON),
		exec.DurationMS,
		formatTime(exec.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert tool_execution %q: %w", exec.ToolExecID, err)
	}
	return nil
}

func (s *SQLiteStore) ListToolExecutions(ctx context.Context, agentRunID string) ([]*domain.ToolExecRecord, error) {
	return listToolExecutions(ctx, s.db, agentRunID)
}

func (s *SQLiteStore) ListToolExecutionsByRun(ctx context.Context, runID string) ([]*domain.ToolExecRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT te.tool_exec_id, te.agent_run_id, te.turn_id, te.tool_call_id, te.tool_name,
		       te.arguments_json, te.output_text, te.exit_code, te.accessed_files_json, te.duration_ms, te.created_at
		FROM tool_executions te
		INNER JOIN agent_runs ar ON ar.agent_run_id = te.agent_run_id
		WHERE ar.run_id = ?
		ORDER BY te.created_at ASC, te.tool_exec_id ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list tool_executions for run %q: %w", runID, err)
	}
	defer rows.Close()
	return scanToolExecutions(rows)
}

func (s *SQLiteStore) SaveCoverageReport(ctx context.Context, report *domain.CoverageReport) error {
	if report == nil {
		return fmt.Errorf("coverage report must not be nil")
	}
	if report.CreatedAt.IsZero() {
		report.CreatedAt = time.Now().UTC()
	}

	unreviewed := report.UnreviewedFiles
	if unreviewed == nil {
		unreviewed = []string{}
	}
	unreviewedJSON, err := json.Marshal(unreviewed)
	if err != nil {
		return fmt.Errorf("marshal unreviewed_files: %w", err)
	}

	details := report.FileDetails
	if details == nil {
		details = []domain.FileCoverageDetail{}
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshal file_coverage_details: %w", err)
	}

	contextFiles := report.ContextFilesRead
	if contextFiles == nil {
		contextFiles = []string{}
	}
	contextFilesJSON, err := json.Marshal(contextFiles)
	if err != nil {
		return fmt.Errorf("marshal out_of_diff_files_read: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO coverage_reports (
			run_id, pr_number, base_sha, head_sha,
			total_changed_files, reviewed_changed_files, file_coverage_ratio,
			total_changed_hunks, reviewed_changed_hunks, hunk_coverage_ratio,
			unreviewed_files_json, file_coverage_details_json, out_of_diff_files_read_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			pr_number = excluded.pr_number,
			base_sha = excluded.base_sha,
			head_sha = excluded.head_sha,
			total_changed_files = excluded.total_changed_files,
			reviewed_changed_files = excluded.reviewed_changed_files,
			file_coverage_ratio = excluded.file_coverage_ratio,
			total_changed_hunks = excluded.total_changed_hunks,
			reviewed_changed_hunks = excluded.reviewed_changed_hunks,
			hunk_coverage_ratio = excluded.hunk_coverage_ratio,
			unreviewed_files_json = excluded.unreviewed_files_json,
			file_coverage_details_json = excluded.file_coverage_details_json,
			out_of_diff_files_read_json = excluded.out_of_diff_files_read_json,
			created_at = excluded.created_at
	`,
		report.RunID,
		report.PRNumber,
		report.BaseSHA,
		report.HeadSHA,
		report.TotalChangedFiles,
		report.ReviewedChangedFiles,
		report.FileCoverageRatio,
		report.TotalChangedHunks,
		report.ReviewedChangedHunks,
		report.HunkCoverageRatio,
		string(unreviewedJSON),
		string(detailsJSON),
		string(contextFilesJSON),
		formatTime(report.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("save coverage_report for %q: %w", report.RunID, err)
	}
	return nil
}

func (s *SQLiteStore) GetCoverageReport(ctx context.Context, runID string) (*domain.CoverageReport, error) {
	return getCoverageReport(ctx, s.db, runID)
}

func (s *SQLiteStore) GetRunDetail(ctx context.Context, runID string) (*store.RunDetail, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read tx for run %q: %w", runID, err)
	}
	defer tx.Rollback()

	run, err := getRun(ctx, tx, runID)
	if err != nil {
		return nil, err
	}

	detail := &store.RunDetail{
		Run: run,
	}

	cs, err := getChangesetSnapshot(ctx, tx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	detail.Changeset = cs

	agentRuns, err := listAgentRuns(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	detail.AgentRuns = make([]*store.AgentRunDetail, 0, len(agentRuns))
	for _, ar := range agentRuns {
		turns, err := listLLMTurns(ctx, tx, ar.AgentRunID)
		if err != nil {
			return nil, err
		}
		execs, err := listToolExecutions(ctx, tx, ar.AgentRunID)
		if err != nil {
			return nil, err
		}
		detail.AgentRuns = append(detail.AgentRuns, &store.AgentRunDetail{
			AgentRun:       ar,
			LLMTurns:       turns,
			ToolExecutions: execs,
		})
	}

	cov, err := getCoverageReport(ctx, tx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	detail.Coverage = cov

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit read tx for run %q: %w", runID, err)
	}

	return detail, nil
}

type queryRunner interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getRun(ctx context.Context, q queryRunner, runID string) (*domain.RunRecord, error) {
	row := q.QueryRowContext(ctx, `
		SELECT run_id, workflow_name, trigger_type, dedup_key, status,
		       resolved_config_yaml, final_artifact_json, error_message, started_at, completed_at
		FROM workflow_runs
		WHERE run_id = ?
	`, runID)
	rec, err := scanRunRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get workflow_run %q: %w", runID, err)
	}
	return rec, nil
}

func getChangesetSnapshot(ctx context.Context, q queryRunner, runID string) (*domain.ChangesetSpec, error) {
	var cs domain.ChangesetSpec
	var changedFilesJSON string
	err := q.QueryRowContext(ctx, `
		SELECT provider, repo_owner, repo_name, pr_number, pr_url,
		       pr_title, pr_description, pr_author, trigger_event, installation_id,
		       base_branch, base_sha, head_branch, head_sha, unified_diff, changed_files_json
		FROM changeset_snapshots
		WHERE run_id = ?
	`, runID).Scan(
		&cs.Provider,
		&cs.RepoOwner,
		&cs.RepoName,
		&cs.PRNumber,
		&cs.PRURL,
		&cs.Title,
		&cs.Description,
		&cs.Author,
		&cs.TriggerEvent,
		&cs.InstallationID,
		&cs.BaseBranch,
		&cs.BaseSHA,
		&cs.HeadBranch,
		&cs.HeadSHA,
		&cs.UnifiedDiff,
		&changedFilesJSON,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get changeset_snapshot for %q: %w", runID, err)
	}
	if err := json.Unmarshal([]byte(changedFilesJSON), &cs.ChangedFiles); err != nil {
		return nil, fmt.Errorf("unmarshal changed_files_json for %q: %w", runID, err)
	}
	return &cs, nil
}

func listAgentRuns(ctx context.Context, q queryRunner, runID string) ([]*domain.AgentRunRecord, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT agent_run_id, run_id, role, provider, model, sandbox_id,
		       rendered_system_prompt, rendered_user_prompt, status, turns_used,
		       input_tokens, output_tokens, cached_tokens, submitted_output_json,
		       error_message, started_at, completed_at
		FROM agent_runs
		WHERE run_id = ?
		ORDER BY started_at ASC, agent_run_id ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("list agent_runs for %q: %w", runID, err)
	}
	defer rows.Close()

	var out []*domain.AgentRunRecord
	for rows.Next() {
		var rec domain.AgentRunRecord
		var statusStr string
		var submittedJSON, completedStr sql.NullString
		var startedStr string
		if err := rows.Scan(
			&rec.AgentRunID,
			&rec.RunID,
			&rec.Role,
			&rec.Provider,
			&rec.Model,
			&rec.SandboxID,
			&rec.RenderedSystemPrompt,
			&rec.RenderedUserPrompt,
			&statusStr,
			&rec.TurnsUsed,
			&rec.InputTokens,
			&rec.OutputTokens,
			&rec.CachedTokens,
			&submittedJSON,
			&rec.ErrorMessage,
			&startedStr,
			&completedStr,
		); err != nil {
			return nil, fmt.Errorf("scan agent_run: %w", err)
		}
		rec.Status = domain.AgentRunStatus(statusStr)
		if submittedJSON.Valid {
			rec.SubmittedOutputJSON = json.RawMessage(submittedJSON.String)
		}
		startedAt, err := parseTime(startedStr)
		if err != nil {
			return nil, fmt.Errorf("parse agent_run started_at: %w", err)
		}
		rec.StartedAt = startedAt
		completedAt, err := parseNullableTime(completedStr)
		if err != nil {
			return nil, fmt.Errorf("parse agent_run completed_at: %w", err)
		}
		rec.CompletedAt = completedAt
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent_runs: %w", err)
	}
	return out, nil
}

func listLLMTurns(ctx context.Context, q queryRunner, agentRunID string) ([]*domain.LLMTurnRecord, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT turn_id, agent_run_id, turn_index, request_messages_json, response_message_json,
		       thinking_trace, tool_calls_json, input_tokens, output_tokens, cached_tokens,
		       latency_ms, created_at
		FROM llm_turns
		WHERE agent_run_id = ?
		ORDER BY turn_index ASC, created_at ASC
	`, agentRunID)
	if err != nil {
		return nil, fmt.Errorf("list llm_turns for %q: %w", agentRunID, err)
	}
	defer rows.Close()

	var out []*domain.LLMTurnRecord
	for rows.Next() {
		var rec domain.LLMTurnRecord
		var reqJSON, respJSON, createdStr string
		var toolCallsJSON sql.NullString
		if err := rows.Scan(
			&rec.TurnID,
			&rec.AgentRunID,
			&rec.TurnIndex,
			&reqJSON,
			&respJSON,
			&rec.ThinkingTrace,
			&toolCallsJSON,
			&rec.InputTokens,
			&rec.OutputTokens,
			&rec.CachedTokens,
			&rec.LatencyMS,
			&createdStr,
		); err != nil {
			return nil, fmt.Errorf("scan llm_turn: %w", err)
		}
		rec.RequestMessagesJSON = json.RawMessage(reqJSON)
		rec.ResponseMessageJSON = json.RawMessage(respJSON)
		if toolCallsJSON.Valid {
			rec.ToolCallsJSON = json.RawMessage(toolCallsJSON.String)
		}
		createdAt, err := parseTime(createdStr)
		if err != nil {
			return nil, fmt.Errorf("parse llm_turn created_at: %w", err)
		}
		rec.CreatedAt = createdAt
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate llm_turns: %w", err)
	}
	return out, nil
}

func listToolExecutions(ctx context.Context, q queryRunner, agentRunID string) ([]*domain.ToolExecRecord, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT tool_exec_id, agent_run_id, turn_id, tool_call_id, tool_name,
		       arguments_json, output_text, exit_code, accessed_files_json, duration_ms, created_at
		FROM tool_executions
		WHERE agent_run_id = ?
		ORDER BY created_at ASC, tool_exec_id ASC
	`, agentRunID)
	if err != nil {
		return nil, fmt.Errorf("list tool_executions for %q: %w", agentRunID, err)
	}
	defer rows.Close()
	return scanToolExecutions(rows)
}

func getCoverageReport(ctx context.Context, q queryRunner, runID string) (*domain.CoverageReport, error) {
	var report domain.CoverageReport
	var unreviewedJSON, detailsJSON, contextFilesJSON, createdStr string

	err := q.QueryRowContext(ctx, `
		SELECT run_id, pr_number, base_sha, head_sha,
		       total_changed_files, reviewed_changed_files, file_coverage_ratio,
		       total_changed_hunks, reviewed_changed_hunks, hunk_coverage_ratio,
		       unreviewed_files_json, file_coverage_details_json, out_of_diff_files_read_json, created_at
		FROM coverage_reports
		WHERE run_id = ?
	`, runID).Scan(
		&report.RunID,
		&report.PRNumber,
		&report.BaseSHA,
		&report.HeadSHA,
		&report.TotalChangedFiles,
		&report.ReviewedChangedFiles,
		&report.FileCoverageRatio,
		&report.TotalChangedHunks,
		&report.ReviewedChangedHunks,
		&report.HunkCoverageRatio,
		&unreviewedJSON,
		&detailsJSON,
		&contextFilesJSON,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get coverage_report for %q: %w", runID, err)
	}

	if err := json.Unmarshal([]byte(unreviewedJSON), &report.UnreviewedFiles); err != nil {
		return nil, fmt.Errorf("unmarshal unreviewed_files_json: %w", err)
	}
	if err := json.Unmarshal([]byte(detailsJSON), &report.FileDetails); err != nil {
		return nil, fmt.Errorf("unmarshal file_coverage_details_json: %w", err)
	}
	if err := json.Unmarshal([]byte(contextFilesJSON), &report.ContextFilesRead); err != nil {
		return nil, fmt.Errorf("unmarshal out_of_diff_files_read_json: %w", err)
	}
	createdAt, err := parseTime(createdStr)
	if err != nil {
		return nil, fmt.Errorf("parse coverage_report created_at: %w", err)
	}
	report.CreatedAt = createdAt
	return &report, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRunRecord(row rowScanner) (*domain.RunRecord, error) {
	var rec domain.RunRecord
	var statusStr, startedStr string
	var finalArtifact, completedStr sql.NullString
	if err := row.Scan(
		&rec.RunID,
		&rec.WorkflowName,
		&rec.TriggerType,
		&rec.DedupKey,
		&statusStr,
		&rec.ResolvedConfigYAML,
		&finalArtifact,
		&rec.ErrorMessage,
		&startedStr,
		&completedStr,
	); err != nil {
		return nil, err
	}
	rec.Status = domain.RunStatus(statusStr)
	if finalArtifact.Valid {
		rec.FinalArtifactJSON = json.RawMessage(finalArtifact.String)
	}
	startedAt, err := parseTime(startedStr)
	if err != nil {
		return nil, fmt.Errorf("parse started_at: %w", err)
	}
	rec.StartedAt = startedAt
	completedAt, err := parseNullableTime(completedStr)
	if err != nil {
		return nil, fmt.Errorf("parse completed_at: %w", err)
	}
	rec.CompletedAt = completedAt
	return &rec, nil
}

func scanToolExecutions(rows *sql.Rows) ([]*domain.ToolExecRecord, error) {
	var out []*domain.ToolExecRecord
	for rows.Next() {
		var rec domain.ToolExecRecord
		var turnID, accessedJSON sql.NullString
		var argsJSON, createdStr string
		if err := rows.Scan(
			&rec.ToolExecID,
			&rec.AgentRunID,
			&turnID,
			&rec.ToolCallID,
			&rec.ToolName,
			&argsJSON,
			&rec.OutputText,
			&rec.ExitCode,
			&accessedJSON,
			&rec.DurationMS,
			&createdStr,
		); err != nil {
			return nil, fmt.Errorf("scan tool_execution: %w", err)
		}
		if turnID.Valid {
			rec.TurnID = turnID.String
		}
		rec.ArgumentsJSON = json.RawMessage(argsJSON)
		if accessedJSON.Valid && accessedJSON.String != "" {
			if err := json.Unmarshal([]byte(accessedJSON.String), &rec.AccessedFiles); err != nil {
				return nil, fmt.Errorf("unmarshal accessed_files_json: %w", err)
			}
		}
		createdAt, err := parseTime(createdStr)
		if err != nil {
			return nil, fmt.Errorf("parse tool_execution created_at: %w", err)
		}
		rec.CreatedAt = createdAt
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tool_executions: %w", err)
	}
	return out, nil
}

func requireOneRowAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(sqliteTimeLayout)
}

func formatNullableTime(t *time.Time) sql.NullString {
	if t == nil || t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(*t), Valid: true}
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

func parseNullableTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullableJSON(raw json.RawMessage) sql.NullString {
	if len(raw) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(raw), Valid: true}
}

func defaultJSON(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

func normalizeRepoCredentialKey(owner, repo, provider string) (string, string, string) {
	return strings.ToLower(strings.TrimSpace(owner)),
		strings.ToLower(strings.TrimSpace(repo)),
		strings.ToLower(strings.TrimSpace(provider))
}

func (s *SQLiteStore) UpsertRepoCredential(ctx context.Context, cred *domain.RepoCredentialRecord) error {
	if cred == nil {
		return fmt.Errorf("credential must not be nil")
	}
	owner, repo, provider := normalizeRepoCredentialKey(cred.RepoOwner, cred.RepoName, cred.Provider)
	if owner == "" || repo == "" || provider == "" {
		return fmt.Errorf("repo_owner, repo_name, and provider are required")
	}
	if len(cred.EncryptedAPIKey) == 0 || len(cred.Nonce) == 0 {
		return fmt.Errorf("encrypted_api_key and nonce are required")
	}
	now := time.Now().UTC()
	createdAt := cred.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := cred.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	baseURL := strings.TrimSpace(cred.BaseURL)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repo_credentials (
			repo_owner, repo_name, provider, encrypted_api_key, nonce, base_url, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo_owner, repo_name, provider) DO UPDATE SET
			encrypted_api_key = excluded.encrypted_api_key,
			nonce = excluded.nonce,
			base_url = excluded.base_url,
			updated_at = excluded.updated_at
	`, owner, repo, provider, cred.EncryptedAPIKey, cred.Nonce, baseURL, formatTime(createdAt), formatTime(updatedAt))
	if err != nil {
		return fmt.Errorf("upsert repo credential %s/%s (%s): %w", owner, repo, provider, err)
	}
	return nil
}

func (s *SQLiteStore) GetRepoCredential(ctx context.Context, owner, repo, provider string) (*domain.RepoCredentialRecord, error) {
	normOwner, normRepo, normProvider := normalizeRepoCredentialKey(owner, repo, provider)
	if normOwner == "" || normRepo == "" || normProvider == "" {
		return nil, fmt.Errorf("repo_owner, repo_name, and provider are required")
	}
	var rec domain.RepoCredentialRecord
	var createdStr, updatedStr string
	err := s.db.QueryRowContext(ctx, `
		SELECT repo_owner, repo_name, provider, encrypted_api_key, nonce, base_url, created_at, updated_at
		FROM repo_credentials
		WHERE repo_owner = ? AND repo_name = ? AND provider = ?
	`, normOwner, normRepo, normProvider).Scan(
		&rec.RepoOwner,
		&rec.RepoName,
		&rec.Provider,
		&rec.EncryptedAPIKey,
		&rec.Nonce,
		&rec.BaseURL,
		&createdStr,
		&updatedStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get repo credential %s/%s (%s): %w", normOwner, normRepo, normProvider, err)
	}
	createdAt, err := parseTime(createdStr)
	if err != nil {
		return nil, fmt.Errorf("parse repo credential created_at: %w", err)
	}
	updatedAt, err := parseTime(updatedStr)
	if err != nil {
		return nil, fmt.Errorf("parse repo credential updated_at: %w", err)
	}
	rec.CreatedAt = createdAt
	rec.UpdatedAt = updatedAt
	return &rec, nil
}

func (s *SQLiteStore) ListRepoCredentials(ctx context.Context, owner, repo string) ([]*domain.RepoCredentialSummary, error) {
	normOwner, normRepo, _ := normalizeRepoCredentialKey(owner, repo, "")
	if normOwner == "" || normRepo == "" {
		return nil, fmt.Errorf("repo_owner and repo_name are required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT repo_owner, repo_name, provider, base_url, length(encrypted_api_key) > 0, created_at, updated_at
		FROM repo_credentials
		WHERE repo_owner = ? AND repo_name = ?
		ORDER BY provider ASC
	`, normOwner, normRepo)
	if err != nil {
		return nil, fmt.Errorf("list repo credentials %s/%s: %w", normOwner, normRepo, err)
	}
	defer rows.Close()

	var out []*domain.RepoCredentialSummary
	for rows.Next() {
		var item domain.RepoCredentialSummary
		var createdStr, updatedStr string
		if err := rows.Scan(
			&item.RepoOwner,
			&item.RepoName,
			&item.Provider,
			&item.BaseURL,
			&item.HasAPIKey,
			&createdStr,
			&updatedStr,
		); err != nil {
			return nil, fmt.Errorf("scan repo credential summary: %w", err)
		}
		createdAt, err := parseTime(createdStr)
		if err != nil {
			return nil, fmt.Errorf("parse repo credential created_at: %w", err)
		}
		updatedAt, err := parseTime(updatedStr)
		if err != nil {
			return nil, fmt.Errorf("parse repo credential updated_at: %w", err)
		}
		item.CreatedAt = createdAt
		item.UpdatedAt = updatedAt
		out = append(out, &item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repo credentials: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) DeleteRepoCredential(ctx context.Context, owner, repo, provider string) error {
	normOwner, normRepo, normProvider := normalizeRepoCredentialKey(owner, repo, provider)
	if normOwner == "" || normRepo == "" || normProvider == "" {
		return fmt.Errorf("repo_owner, repo_name, and provider are required")
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM repo_credentials
		WHERE repo_owner = ? AND repo_name = ? AND provider = ?
	`, normOwner, normRepo, normProvider)
	if err != nil {
		return fmt.Errorf("delete repo credential %s/%s (%s): %w", normOwner, normRepo, normProvider, err)
	}
	return requireOneRowAffected(res)
}
