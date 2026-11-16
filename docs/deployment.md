# Deployment and operations

Mochi needs a continuously running server, persistent SQLite storage, a Daytona
account, and credentials for the configured LLM providers. The supplied image
works with [Compose](../compose.yaml) or the [Render Blueprint](../render.yaml).
Use one server instance: the queue and cancellation state live in memory.

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
| Contents | Read | Source files and read-only clone tokens |
| Pull requests | Read and write | PR metadata, reviews, and inline comments |
| Checks | Read and write | Check Runs |
| Issues | Read | Issue comment event subscription |

Subscribe to **Pull request** and **Issue comment** events. Record the numeric
App ID, download a generated private key, and install the App on the repositories
to review. Mochi obtains the installation ID from webhook events. After changing
App permissions, approve the updated permissions at the installation.

Both presets review open, non-draft PRs on `opened`, `synchronize`, `reopened`,
and `ready_for_review`. An owner, member, or collaborator can comment
`/mochi review` to rerun a review. For comment-only triggering, disable the App's
Pull request event subscription. `events: []` restores the default events rather
than disabling them.

## Prepare Daytona snapshots

Create a Daytona API key, then use the Daytona CLI from the repository root:

```sh
daytona login
daytona snapshot create mochi-review-go \
  --dockerfile deploy/sandbox/go.Dockerfile \
  --cpu 1 --memory 2
daytona snapshot list
```

Wait until the snapshot is available. The full preset uses `mochi-review-go`,
which includes Go and basic review tools. The small preset uses `mochi-review`;
create it from `deploy/sandbox/Dockerfile` instead. Add the target repository's
language runtimes and build tools when needed. If you change a snapshot name,
update `sandbox.default_snapshot` in the workflow too.

Changing a Dockerfile under `deploy/sandbox/` does not update an existing
snapshot. Delete the snapshot and create it again under the same name — the
workflow references snapshots by name, so no configuration change is needed.
Wait until the recreated snapshot is active, then smoke-test it once by
starting a sandbox from it and checking the toolchain (for example
`go version`) before relying on it for reviews.

The full preset creates five independent sandboxes per concurrent workflow; the
small preset creates one. Ensure your Daytona quota permits this concurrency and
allocated resources. Both presets disable forking and work with container
snapshots. Enabling `fork_from_base` requires a compatible VM snapshot; see
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
| `MOCHI_API_TOKEN` | Separate random token for inspection, cancellation, and generic triggers |
| `DAYTONA_API_KEY` | Daytona API key |
| `GEMINI_API_KEY` | Gemini key for both presets; `GOOGLE_API_KEY` is an alternative |
| `DAYTONA_TARGET` | Optional Daytona target |
| `PORT` | Container HTTP port; defaults to `8080` |
| `MOCHI_DB_PATH` | SQLite path; container default is `/data/mochi.db` |
| `MOCHI_WORKFLOW` | Container workflow path; defaults to `configs/workflows/pr-review.yaml` |
| `MOCHI_WORKERS` | Container workflow concurrency; defaults to `1` |
| `MOCHI_QUEUE_SIZE` | Container scheduler queue capacity; defaults to `16` |

Generate the webhook secret and API token separately with `openssl rand -hex 32`.
The Render Blueprint generates the API token and requests the remaining required
secrets. For Compose, retain the single quotes around the multiline PEM shown in
`.env.example`; in a hosting secret field, paste the PEM without the outer quotes.
Alternatively, set `MOCHI_GITHUB_PRIVATE_KEY_PATH` to a mounted PEM file instead of
setting `MOCHI_GITHUB_PRIVATE_KEY`.

GitHub App deployments do not need `GITHUB_TOKEN` or `GH_TOKEN`. Both presets use
Gemini only; other provider credentials are described in
[configuration](configuration.md#llm-providers). `GEMINI_API_KEY` takes precedence
over `GOOGLE_API_KEY` when both are set.

`MOCHI_WORKFLOW`, `MOCHI_WORKERS`, `MOCHI_QUEUE_SIZE`, and `PORT` are interpreted by
the container entrypoint. For a standalone binary, use the corresponding
[`serve` flags](usage.md#start-the-server). The binary does not load `.env` files.

## Start with Docker Compose

After configuring secrets and preparing the snapshot:

```sh
docker compose build
docker compose run --rm mochi validate --runtime configs/workflows/pr-review.yaml
docker compose up -d
curl --fail http://localhost:8080/readyz
docker compose logs --tail=100 mochi
```

`validate --runtime` checks local configuration, provider credential presence,
GitHub authentication setup, and HTTP authentication settings. It makes no
external API calls and does not verify key validity, model access, snapshot
availability, or remote permissions. The entrypoint runs it before serving.

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

Enter the required secrets, deploy, and verify `/readyz` returns HTTP 200. Update
the GitHub App webhook URL to the public HTTPS URL plus `/webhooks/github`, and
activate delivery. Automatic deployment is disabled by the Blueprint; release
subsequent versions with Manual Deploy.

To select another preset, first prepare its snapshot, then update
`MOCHI_WORKFLOW` and redeploy while no reviews are running or queued. Existing
service environment values take precedence over image defaults. On other hosts,
provide the same persistent storage and continuously running single instance.

## Verify a deployment

Redeliver a GitHub App ping and confirm an HTTP 200 `ignored` response. Then open
a small non-draft PR and check that a review starts automatically. Add a commit
to verify reruns; verify drafts are skipped and marking a PR ready starts a review.
These checks call external APIs and publish to the test PR.

Confirm the Check Run and review identify the intended head SHA, all expected
specialist results are present, and their sandboxes are removed after completion.
For the test runner, inspect `test_execution` and any `skipped_reasons`; a
completed report does not prove tests ran. Verify unauthenticated
`GET /api/v1/runs` returns 401 and an authenticated request succeeds. After a
redeploy, confirm past runs remain available through the API or `mochi inspect`.

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
