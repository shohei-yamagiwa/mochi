# Deployment and operations

Mochi needs a continuously running server, persistent SQLite storage, a shared
Daytona account, a 32-byte `MOCHI_MASTER_KEY`, and per-repository LLM API keys
registered in Mochi. The supplied image works with [Compose](../compose.yaml) or
the [Render Blueprint](../render.yaml). Use one server instance: the queue and
cancellation state live in memory.

## Configure a GitHub App

Create an App under GitHub **Settings → Developer settings → GitHub Apps**.
Set its webhook URL to `https://<your-host>/webhooks/github`, enable SSL
verification, and set a secret generated with `openssl rand -hex 32`.
If the server is not deployed yet, leave delivery inactive until its HTTPS
endpoint is available. User OAuth and a callback URL are not needed.

Grant these repository permissions:

| Permission | Access | Purpose |
| --- | --- | --- |
| Metadata | Read | Repository metadata |
| Contents | Read | Repository workflows, custom prompts, and clone tokens |
| Pull requests | Read and write | PR metadata, reviews, and inline comments |
| Checks | Read and write | Check Runs |
| Issues | Read | Issue comment event subscription |

Subscribe to **Pull request** and **Issue comment** events. Record the numeric
App ID, download a generated private key, and install the App on the repositories
to review. Mochi obtains the installation ID from webhook events. After changing
App permissions, approve the updated permissions at the installation.

Each installed repository configures its own trigger rules under
`.mochi/workflows/*.yaml` on its base branch. To enable comment-only triggering
for a workflow without automatic PR events, set `events: []` in that workflow's
`input.triggers`.

## Prepare Daytona snapshots

Create a shared Daytona API key for the server, then use the Daytona CLI from
the repository root:

```sh
daytona login
daytona snapshot create mochi-review-go \
  --dockerfile deploy/sandbox/go.Dockerfile \
  --cpu 1 --memory 2
daytona snapshot list
```

Wait until the snapshot is available. This repository's full workflow uses
`mochi-review-go` (Go and basic review tools), and its small workflow uses
`mochi-review` (`deploy/sandbox/Dockerfile`). Add each repository's language
runtimes and build tools when needed, and reference the snapshot name in that
repository's `.mochi/workflows/*.yaml` (`sandbox.default_snapshot`).

Changing a Dockerfile under `deploy/sandbox/` does not update an existing
snapshot. Delete the snapshot and create it again under the same name — workflows
reference snapshots by name, so no configuration change is needed. Wait until the
recreated snapshot is active, then smoke-test it once by starting a sandbox from
it and checking the toolchain (for example `go version`).

Ensure your Daytona quota permits the specialist concurrency configured by your
repositories' workflows (`fork_from_base: false` creates one sandbox per
sandbox-enabled specialist). Enabling `fork_from_base` requires a compatible VM
snapshot; see
[Daytona's sandbox documentation](https://www.daytona.io/docs/en/sandboxes/).

Use the Dockerfiles under `deploy/sandbox/` for snapshots; the root `Dockerfile`
builds the Mochi server. Keep secrets out of snapshots. Choose a Daytona target
near the server where available.

## Set environment variables

Use hosting secrets or, for Compose, copy [`.env.example`](../.env.example) to
`.env`, run `chmod 600 .env`, and replace all placeholders.

| Variable | Purpose |
| --- | --- |
| `MOCHI_GITHUB_APP_ID` | Numeric GitHub App ID, not the Client ID |
| `MOCHI_GITHUB_PRIVATE_KEY` | Full PEM private key with actual line breaks |
| `GITHUB_WEBHOOK_SECRET` | Same secret as the GitHub App webhook |
| `MOCHI_API_TOKEN` | Random bearer token for credential management, inspection, cancellation, and generic triggers |
| `MOCHI_MASTER_KEY` | 32-byte key (`openssl rand -hex 32` or base64) used to encrypt per-repository LLM API keys in SQLite |
| `DAYTONA_API_KEY` | Shared Daytona API key |
| `DAYTONA_TARGET` | Optional Daytona target |
| `PORT` | Container HTTP port; defaults to `8080` |
| `MOCHI_DB_PATH` | SQLite path; container default is `/data/mochi.db` |
| `MOCHI_WORKERS` | Container workflow concurrency; defaults to `1` |
| `MOCHI_QUEUE_SIZE` | Container scheduler queue capacity; defaults to `16` |
| `MOCHI_WORKFLOW` | Optional comma-separated static workflow path(s); leave unset for per-repo `.mochi/workflows/*.yaml` mode |

Generate `GITHUB_WEBHOOK_SECRET`, `MOCHI_API_TOKEN`, and `MOCHI_MASTER_KEY`
separately with `openssl rand -hex 32`. Keep a secure backup of `MOCHI_MASTER_KEY`
alongside your SQLite backups: without it, stored per-repository API keys cannot
be decrypted.

The Render Blueprint generates `MOCHI_API_TOKEN` and requests the remaining
required secrets. For Compose, retain the single quotes around the multiline PEM
shown in `.env.example`; in a hosting secret field, paste the PEM without the
outer quotes. Alternatively, set `MOCHI_GITHUB_PRIVATE_KEY_PATH` to a mounted PEM
file instead of setting `MOCHI_GITHUB_PRIVATE_KEY`.

Server-level LLM provider environment variables (`GEMINI_API_KEY`,
`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`) are **not** used by `mochi serve`. Every
repository must have its own provider API key registered in Mochi.

`MOCHI_WORKERS`, `MOCHI_QUEUE_SIZE`, and `PORT` are interpreted by the container
entrypoint. For a standalone binary, use the corresponding
[`serve` flags](usage.md#start-the-server). The binary does not load `.env` files.

## Register per-repository LLM API keys

After starting the server, register an encrypted API key for each repository and
each provider referenced by its `.mochi/workflows/*.yaml`:

```sh
# Via the HTTP API on the deployed server:
./mochi credentials set owner/repo gemini \
  --server https://mochi.example.com \
  --api-token "$MOCHI_API_TOKEN" \
  --api-key-stdin

# Or directly against the container's local SQLite database:
docker compose exec -T mochi mochi credentials set owner/repo gemini --api-key-stdin
```

Inspect configured providers (keys are never returned) or remove a key:

```sh
./mochi credentials list owner/repo --server https://mochi.example.com --api-token "$MOCHI_API_TOKEN"
./mochi credentials delete owner/repo gemini --server https://mochi.example.com --api-token "$MOCHI_API_TOKEN"
```

## Start with Docker Compose

After configuring secrets and preparing the snapshot:

```sh
docker compose build
docker compose run --rm mochi validate --runtime
docker compose up -d
curl --fail http://localhost:8080/readyz
docker compose logs --tail=100 mochi
```

`validate --runtime` (without workflow paths) checks server deployment
readiness: GitHub authentication setup, HTTP webhook/API auth secrets, shared
Daytona credentials, and `MOCHI_MASTER_KEY`. It makes no external API calls. The
container entrypoint runs it before serving.

Compose binds to localhost. Configure a public HTTPS reverse proxy or tunnel to
receive GitHub webhooks. SQLite is stored in the `mochi-data` volume. The entrypoint
initializes `/data` ownership as root, then runs validation and the server as
UID/GID `10001:10001`.

## Deploy on Render

Connect your repository to Render and create a Blueprint using `render.yaml`.
It targets `main`, runs one paid web service, mounts a persistent disk at `/data`,
and uses `/readyz` for health checks. Review current
[Render pricing](https://render.com/pricing), region, and disk capacity before
creating the service; LLM and Daytona usage are billed separately.

Enter the required secrets (including `MOCHI_MASTER_KEY`), deploy, and verify
`/readyz` returns HTTP 200. Update the GitHub App webhook URL to the public HTTPS
URL plus `/webhooks/github`, and activate delivery. Then register per-repository
API keys against `https://<your-render-service>.onrender.com` using
`mochi credentials set --server ...` with `MOCHI_API_TOKEN`.

The Blueprint enables automatic deployments after the linked `main` commit's CI
checks pass (`autoDeployTrigger: checksPass`). Pushes to `develop` run CI but do
not deploy production.

### Enable continuous deployment on an existing service

After merging this configuration, open the existing service in the Render
Dashboard and set **Settings > Auto-Deploy > After CI Checks Pass**, then save.
The Blueprint has automatic syncing disabled in the current deployment, so
merging `render.yaml` alone does not update the live service setting. The Dashboard
setting and this file must both use the CI-gated mode so a future Blueprint sync
does not disable it. Alternatively, review and apply a Blueprint sync after the
merge. No deploy hook or additional GitHub Actions secret is required.

Render must have a connected Git provider for automatic deploys. If the setting
is unavailable for a public-repository URL deployment, connect the GitHub
repository in Render first. All checks on the linked commit must pass; Render
does not deploy when no checks are found or any check fails. The existing CI
workflow already runs on `main` pushes.

Automatic deployment does not wait for Mochi's review queue to become idle.
The persistent disk requires a brief interruption when swapping instances, and
in-flight reviews or queued work can be interrupted. Merge production releases
while reviews are idle and retrigger interrupted runs as described below. If a
maintenance window is required, temporarily set Auto-Deploy to **Off**, deploy
manually, and restore **After CI Checks Pass** afterward.

For the first release after enabling CD, confirm the new `main` commit's CI
succeeds, a deploy starts without Manual Deploy, and that same commit becomes
`live` with `/readyz` returning HTTP 200. Snapshot image changes still require
separate creation in Daytona; Render CD does not rebuild Daytona snapshots.

## Verify a deployment

Redeliver a GitHub App ping and confirm an HTTP 200 `ignored` response. Then open
a small non-draft PR targeting a branch matched by your repository's
`.mochi/workflows/*.yaml` and check that a review starts automatically. Add a
commit to verify reruns; verify drafts are skipped and marking a PR ready starts
a review. These checks call external APIs and publish to the test PR.

Confirm the Check Run (`Mochi Code Review (<workflow_name>)`) and review
identify the intended head SHA, all expected specialist results are present, and
their sandboxes are removed after completion. For the test runner, inspect
`test_execution` and any `skipped_reasons`; a completed report does not prove
tests ran. Verify unauthenticated `GET /api/v1/runs` and
`GET /api/v1/repos/{owner}/{repo}/credentials` return 401 and authenticated
requests succeed. After a redeploy, confirm past runs and encrypted credentials
remain available.

HTTP 202 acknowledges a webhook; it does not mean review completion. Inspect logs,
Check Runs, and persisted runs for later failures. `/readyz` checks scheduler
presence and database access, not external API health. See [usage](usage.md) for
inspection commands.

## Operations and recovery

Deploy while the queue is empty and reviews are idle. Webhook context fetching
and queued jobs are held in memory; a forced stop can lose accepted work. Runs
left `pending` or `running` are not automatically resumed. Inspect GitHub delivery
history and persisted runs, then redeliver or retrigger affected reviews.
Deduplication cancels superseded work for a repository/PR; it does not permanently
suppress repeated deliveries, so redelivery can incur new API usage.

Services using a Render disk can have deployment downtime; see
[Render disk constraints](https://render.com/docs/disks). Check failed deliveries
and redeliver them as needed. Monitor database growth, server memory, provider
quotas, and remaining Daytona sandboxes. Mochi attempts sandbox deletion and sets
a 15-minute auto-stop interval, but a server crash can leave external resources.
Token budgets are checked after model calls and are not exact spending caps.

The database contains diffs, model messages, and tool outputs, potentially
including private source code. Restrict access to the API and backups. There is no
automatic data retention cleanup; plan storage capacity and retention accordingly.

For a live SQLite backup, use SQLite's backup operation rather than copying the
database file without its WAL. The server image includes `sqlite3`:

```sh
umask 077
mkdir -p /data/backups
sqlite3 /data/mochi.db ".backup '/data/backups/mochi-backup.db'"
sqlite3 /data/backups/mochi-backup.db 'PRAGMA integrity_check;'
```

Copy the backup to protected storage outside the service disk. To restore, stop
the server, preserve the existing database and WAL/SHM files separately, and place
the backup at the configured database path. Set ownership to `10001:10001` for the
container, run `integrity_check`, restart, and verify historical runs. Before
reverting an application release, check database migration compatibility; a code
rollback alone may not restore compatibility.
