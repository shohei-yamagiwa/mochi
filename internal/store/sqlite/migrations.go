package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

type migration struct {
	version int
	name    string
	sql     string
}

var migrations = []migration{
	{
		version: 1,
		name:    "initial_audit_and_coverage_schema",
		sql: `
CREATE TABLE IF NOT EXISTS workflow_runs (
	run_id TEXT PRIMARY KEY,
	workflow_name TEXT NOT NULL,
	trigger_type TEXT NOT NULL,
	dedup_key TEXT NOT NULL,
	status TEXT NOT NULL,
	resolved_config_yaml TEXT NOT NULL DEFAULT '',
	final_artifact_json TEXT,
	error_message TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL,
	completed_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_workflow_runs_dedup_key ON workflow_runs (dedup_key);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_started_at ON workflow_runs (started_at DESC);

CREATE TABLE IF NOT EXISTS changeset_snapshots (
	run_id TEXT PRIMARY KEY REFERENCES workflow_runs (run_id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	repo_owner TEXT NOT NULL,
	repo_name TEXT NOT NULL,
	pr_number INTEGER NOT NULL DEFAULT 0,
	pr_url TEXT NOT NULL DEFAULT '',
	pr_title TEXT NOT NULL DEFAULT '',
	pr_description TEXT NOT NULL DEFAULT '',
	pr_author TEXT NOT NULL DEFAULT '',
	trigger_event TEXT NOT NULL DEFAULT '',
	installation_id INTEGER NOT NULL DEFAULT 0,
	base_branch TEXT NOT NULL DEFAULT '',
	base_sha TEXT NOT NULL DEFAULT '',
	head_branch TEXT NOT NULL DEFAULT '',
	head_sha TEXT NOT NULL DEFAULT '',
	unified_diff TEXT NOT NULL DEFAULT '',
	changed_files_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_runs (
	agent_run_id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES workflow_runs (run_id) ON DELETE CASCADE,
	role TEXT NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	sandbox_id TEXT NOT NULL DEFAULT '',
	rendered_system_prompt TEXT NOT NULL DEFAULT '',
	rendered_user_prompt TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	turns_used INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	submitted_output_json TEXT,
	error_message TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL,
	completed_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_agent_runs_run_id ON agent_runs (run_id);

CREATE TABLE IF NOT EXISTS llm_turns (
	turn_id TEXT PRIMARY KEY,
	agent_run_id TEXT NOT NULL REFERENCES agent_runs (agent_run_id) ON DELETE CASCADE,
	turn_index INTEGER NOT NULL,
	request_messages_json TEXT NOT NULL,
	response_message_json TEXT NOT NULL,
	thinking_trace TEXT NOT NULL DEFAULT '',
	tool_calls_json TEXT,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_llm_turns_agent_run_id ON llm_turns (agent_run_id, turn_index);

CREATE TABLE IF NOT EXISTS tool_executions (
	tool_exec_id TEXT PRIMARY KEY,
	agent_run_id TEXT NOT NULL REFERENCES agent_runs (agent_run_id) ON DELETE CASCADE,
	turn_id TEXT REFERENCES llm_turns (turn_id) ON DELETE SET NULL,
	tool_call_id TEXT NOT NULL,
	tool_name TEXT NOT NULL,
	arguments_json TEXT NOT NULL,
	output_text TEXT NOT NULL DEFAULT '',
	exit_code INTEGER NOT NULL DEFAULT 0,
	accessed_files_json TEXT,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tool_executions_agent_run_id ON tool_executions (agent_run_id, created_at);

CREATE TABLE IF NOT EXISTS coverage_reports (
	run_id TEXT PRIMARY KEY REFERENCES workflow_runs (run_id) ON DELETE CASCADE,
	pr_number INTEGER NOT NULL DEFAULT 0,
	base_sha TEXT NOT NULL DEFAULT '',
	head_sha TEXT NOT NULL DEFAULT '',
	total_changed_files INTEGER NOT NULL DEFAULT 0,
	reviewed_changed_files INTEGER NOT NULL DEFAULT 0,
	file_coverage_ratio REAL NOT NULL DEFAULT 0,
	total_changed_hunks INTEGER NOT NULL DEFAULT 0,
	reviewed_changed_hunks INTEGER NOT NULL DEFAULT 0,
	hunk_coverage_ratio REAL NOT NULL DEFAULT 0,
	unreviewed_files_json TEXT NOT NULL,
	file_coverage_details_json TEXT NOT NULL,
	out_of_diff_files_read_json TEXT NOT NULL,
	created_at TEXT NOT NULL
);
`,
	},
	{
		version: 2,
		name:    "repo_credentials",
		sql: `
CREATE TABLE IF NOT EXISTS repo_credentials (
	repo_owner TEXT NOT NULL,
	repo_name TEXT NOT NULL,
	provider TEXT NOT NULL,
	encrypted_api_key BLOB NOT NULL,
	nonce BLOB NOT NULL,
	base_url TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (repo_owner, repo_name, provider)
);

CREATE INDEX IF NOT EXISTS idx_repo_credentials_repo ON repo_credentials (repo_owner, repo_name);
`,
	},
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
	`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	for _, m := range migrations {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, m.version).Scan(&count); err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if count > 0 {
			continue
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d tx: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d (%s): %w", m.version, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d (%s): %w", m.version, m.name, err)
		}
	}

	return nil
}
