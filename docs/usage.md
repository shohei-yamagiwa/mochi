# Usage

Build the CLI with `go build -o mochi ./cmd/mochi` from the repository root. The
examples below use that binary. Run `./mochi <command> --help` for all flags.
For credentials and sandbox preparation, see [deployment](deployment.md).

## Validate configuration

```sh
./mochi validate configs/workflows/pr-review.yaml configs/workflows/pr-review-small.yaml
./mochi validate --runtime configs/workflows/pr-review.yaml
```

Plain validation checks workflow structure, tool names, and prompt templates
without credentials. Runtime validation also checks local credentials and server
authentication configuration; neither command calls external APIs. Prompt files
resolve relative to an automatically detected project root, or `--base-dir`.

## Review a GitHub pull request

Export `GEMINI_API_KEY` and `DAYTONA_API_KEY` for the bundled presets, and configure
GitHub App credentials or a token in `GITHUB_TOKEN` (`GH_TOKEN` is also accepted).
The token needs access to the repository; publishing also requires permissions
for reviews and Check Runs. Prepare the workflow's Daytona snapshot first.

```sh
./mochi run --pr 'owner/repo#42' --dry-run
./mochi run --pr 'owner/repo#42'
./mochi run --pr 'owner/repo#42' --json --output review.json
./mochi run --pr 'owner/repo#42' --post-github
```

`--pr` also accepts a GitHub PR URL. CLI runs suppress the GitHub review sink by
default and print a console report when no other sinks remain. Use `--post-github`
to publish reviews and Check Runs. Other configured sinks remain active.
`--json` prints the final artifact; `--output` also saves it to a file. Use a
workflow with only the desired sinks when stdout must contain JSON alone.

`--dry-run` resolves the workflow and changeset without starting LLMs, sandboxes,
or output sinks. A PR dry run still authenticates to GitHub and fetches PR data.

## Review a local commit range

```sh
./mochi run --repo /path/to/repository --base main --head HEAD --dry-run
./mochi run --repo /path/to/repository --base main --head HEAD
```

The CLI compares committed refs; it does not include uncommitted changes. Without
`--base`, it uses `HEAD~1`, or the empty tree for a single-commit repository.
The bundled workflows execute remotely in Daytona: the origin URL and reviewed
commits must be accessible there. Local files and unpushed commits are not uploaded.
For private repositories, prefer `--pr` so GitHub authentication supplies the
clone token.

Use `--workflow` to choose a preset or custom YAML. Runs persist to `mochi.db` by
default (`MOCHI_DB_PATH` overrides this); `--db ''` disables persistence for a CLI
run. `--fail-on-changes` exits with code 2 when the final verdict is
`REQUEST_CHANGES`; execution errors exit with code 1.

## Start the server

After exporting credentials and HTTP authentication secrets:

```sh
./mochi serve --addr :8080 \
  --workflow configs/workflows/pr-review.yaml \
  --db mochi.db --workers 1 --queue-size 16
```

The standalone command defaults to four workers and a queue of 128; the container
entrypoint defaults to one worker and a queue of 16. Workers count concurrent
workflows, not specialists. Multiple workflow files can be loaded with repeated
`--workflow` flags; give them unique `name` values. Both bundled presets are named
`pr-review`, so select one or rename a copy before loading both. The first loaded
workflow is the default for GitHub webhooks.

Configure `GITHUB_WEBHOOK_SECRET` for GitHub delivery and `MOCHI_API_TOKEN` for run
inspection and cancellation. Generic triggers use `MOCHI_TRIGGER_SECRET` when
set, otherwise `MOCHI_API_TOKEN`. `--insecure-no-auth` permits omitted secrets
for local development only. The standalone binary does not load `.env`.

## Inspect and cancel runs

```sh
./mochi inspect --db mochi.db
./mochi inspect RUN_ID --db mochi.db --json
```

The run list supports `--workflow`, `--status`, and `--limit`. Details include
changeset metadata, agent runs, LLM turns, tool calls, coverage, and the artifact.
Use the actual run ID in place of `RUN_ID`.

The server exposes these endpoints:

| Method | Path | Purpose | Authentication |
| --- | --- | --- | --- |
| GET | `/healthz` | Process liveness | None |
| GET | `/readyz` | Scheduler presence and database access | None |
| POST | `/webhooks/github` | GitHub App delivery | GitHub HMAC signature |
| POST | `/api/v1/triggers/{workflow}` | Generic JSON trigger | Trigger secret or API token |
| GET | `/api/v1/runs` | List runs; filters: `workflow`, `status`, `limit` | API bearer token |
| GET | `/api/v1/runs/{run_id}` | Full run detail | API bearer token |
| GET | `/api/v1/runs/{run_id}/coverage` | Coverage report | API bearer token |
| POST | `/api/v1/runs/{run_id}/cancel` | Cancel an active or queued run | API bearer token |

`/api/v1/webhooks/github` is an alias for `/webhooks/github`. Example inspection
and cancellation requests, with `MOCHI_API_TOKEN` exported in your shell:

```sh
curl --fail -H "Authorization: Bearer $MOCHI_API_TOKEN" \
  'http://localhost:8080/api/v1/runs?limit=10'
curl --fail -H "Authorization: Bearer $MOCHI_API_TOKEN" \
  http://localhost:8080/api/v1/runs/RUN_ID/coverage
curl --fail -X POST -H "Authorization: Bearer $MOCHI_API_TOKEN" \
  -H 'Content-Type: application/json' -d '{"reason":"cancel requested"}' \
  http://localhost:8080/api/v1/runs/RUN_ID/cancel
```

## Generic HTTP triggers

Send a JSON payload to `/api/v1/triggers/{workflow}`, using a loaded workflow's
`name`. The payload accepts workspace settings (`repo_url`, `ref`, `base_ref`,
`snapshot`, `image`), `changeset`, `variables`, `dedup_key`, and `sink_metadata`.
The full payload type is
[`TriggerEvent`](../internal/input/webhook/adapter.go); changeset and workspace
fields are defined in [`TaskContext`](../internal/domain/context.go).

Configure prompts and sinks suited to the supplied context. The bundled GitHub
sink expects PR metadata; a generic payload containing only variables is better
paired with a sandbox-free workflow and console, JSON file, or webhook output.

When `MOCHI_TRIGGER_SECRET` is set, authenticate with its bearer token or
`X-Mochi-Signature-256: sha256=<hex HMAC-SHA256 of the raw body>`. Otherwise, use
the API bearer token. Successful enqueueing returns HTTP 202 and a run ID; inspect
the run API for the outcome.

## Interpret results

A review includes a summary, verdict (`APPROVE`, `REQUEST_CHANGES`, or `COMMENT`),
findings, specialist summaries, and diff coverage. Partial reviews include
`failed_agents`. Test results appear in `test_execution`; inspect `ran`, `passed`,
commands, and `skipped_reasons` before treating a review as test verification.

Coverage measures recorded access to changed files and hunks, not the absence of
bugs or test coverage. Check uncovered files and failed specialists before relying
on a result. The presets use `comment_only`: the GitHub sink publishes comments
rather than approval or change-request decisions. See
[configuration](configuration.md#output-sinks) for output controls.
