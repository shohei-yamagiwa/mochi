# Architecture

Mochi separates input adapters, workflow configuration, agent execution, and
output delivery. The executable wires these pieces together in
[`cmd/mochi/main.go`](../cmd/mochi/main.go). Dependency versions are maintained in
[`go.mod`](../go.mod).

## Execution flow

1. An input adapter turns a GitHub event, CLI request, or generic HTTP payload into
   a `TaskContext`: workspace settings, changeset metadata and patches, template
   variables, and output routing metadata.
2. In server mode, the scheduler persists a pending run and its changeset, then
   queues it in memory. A newer run with the same deduplication key cancels the
   older queued or running review. CLI runs call the runner directly.
3. The runner starts output sinks, loads permitted repository overrides from the
   base branch, and persists the resolved workflow configuration.
4. Specialists run concurrently. With the bundled `fork_from_base: false`
   configuration, each sandbox-enabled specialist creates a Daytona sandbox and
   checks out the same head commit independently. The optional fork path prepares
   a shared base and forks it; it requires a compatible Daytona VM snapshot.
5. Each specialist runs a bounded LLM/tool loop. Tools execute in its sandbox;
   model API calls execute in the Mochi process. Findings are submitted through
   `submit_output`. Sandbox cleanup is attempted when the specialist exits.
6. The runner computes diff coverage from recorded tool access, then passes the
   specialist results and coverage to the synthesizer. The synthesizer has no
   sandbox or execution tools and returns a schema-validated review artifact.
7. The runner attaches deterministic coverage and run metadata, persists the final
   artifact, and asks output sinks to publish it.

A specialist failure is normally recorded and passed to synthesis so the remaining
results can still produce a report. A published report therefore does not imply
that every specialist succeeded. Publishing errors are returned; if every started
sink fails to publish, the persisted run is marked failed.

## Package responsibilities

| Package | Responsibility |
| --- | --- |
| `internal/domain` | Task, workflow, run, file access, and coverage types |
| `internal/config` | Strict YAML loading, repository override rules, and Go prompt templates |
| `internal/input` | Diff parsing and GitHub, CLI, and generic webhook adapters |
| `internal/server` | Webhook handling, asynchronous GitHub context fetching, authenticated run APIs, and health endpoints |
| `internal/orchestrator` | Scheduling, specialist and synthesizer execution, budgets, cancellation, and coverage |
| `internal/llm` | Provider interface and OpenAI, Anthropic, Gemini, and OpenAI-compatible implementations |
| `internal/sandbox` | Sandbox interface and Daytona implementation |
| `internal/tools` | Role-scoped tools for files, search, commands, diffs, and structured findings |
| `internal/schema` | JSON Schema inference and validation |
| `internal/output` | Review artifact contract and GitHub, console, JSON file, and webhook sinks |
| `internal/store` | Persistence interface and SQLite implementation |

## Persistence and coverage

SQLite is the implemented storage backend. Opening the store applies migrations,
enables WAL for file databases, and uses a single database connection. Records
include runs, resolved configuration, changesets, agent runs, rendered prompts,
LLM turns, tool executions, coverage, and final artifacts. Inspect these through
the [CLI or HTTP API](usage.md#inspect-and-cancel-runs).

Coverage is computed in
[`internal/orchestrator/coverage.go`](../internal/orchestrator/coverage.go) from
changed files/hunks and successful tool execution records. Reads and diffs mark
files as inspected and cover overlapping hunks; findings contribute citations.
Search hits record ranges for visibility but do not alone mark files or hunks as
reviewed. Unchanged files read for context are reported separately. Coverage
measures recorded inspection, not review correctness or test coverage.

The scheduler and webhook context-fetching tasks are in memory. Persisted runs
are audit records, not a durable job queue; interrupted work is not resumed on
startup. Operate one server instance with persistent SQLite storage. See
[operations](deployment.md#operations-and-recovery) for recovery and backups.

## Configuration and trust boundaries

Server-owned workflows select providers, models, tools, budgets, and sinks.
Repository overrides are loaded from the PR base branch and merged through an
explicit allowlist. Custom prompts use a separate file root under `.mochi/`;
prompt reads use `os.Root` to confine paths. Prompt contexts omit clone tokens and
sensitive sink routing settings. See [configuration](configuration.md).

GitHub webhooks use HMAC verification. Inspection and cancellation APIs use a
bearer token; generic triggers use a separate secret or the API token. GitHub App
installation tokens used for cloning are restricted to repository contents read
access. The sandbox provider removes git credentials before returning the
prepared sandbox. LLM credentials remain in the server process.

Tool allowlists restrict capabilities exposed to each role. Roles with
`exec_command` can run shell commands, so removing `write_file` does not make a
role read-only. Budgets bound turns, tokens, and time; token accounting after
calls does not impose an exact spending cap.

## Extending Mochi

Implement the corresponding interface in `internal/input`, `internal/llm`,
`internal/sandbox`, `internal/tools`, `internal/output`, or `internal/store`, and
wire it into the executable's adapter or registry setup. Add focused tests for the
new contract and its failure behavior. New LLM provider names also need to be
accepted by configuration and runtime validation.

For new review roles, start with the existing YAML and prompt files. Workflow
definitions are configurable, but the runner currently uses specialist findings
and a `ReviewReport` contract; a different artifact type requires code changes.
See [contributing](../CONTRIBUTING.md) for development checks.
