# mochi

Mochi is a Go server and CLI for agent workflows. It runs as a GitHub App
server that discovers review workflows from each installed repository's
`.mochi/workflows/` directory, isolates tool execution in Daytona sandboxes, uses
per-repository encrypted LLM API keys, and stores reports, execution traces, and
diff coverage in SQLite.

## Get started

Follow the [deployment guide](docs/deployment.md) to configure a GitHub App,
`MOCHI_MASTER_KEY`, a shared Daytona snapshot/API key, and per-repository LLM API
keys. Then start the server with Docker Compose:

```sh
cp .env.example .env
chmod 600 .env
# Replace the placeholders in .env before continuing.
docker compose build
docker compose run --rm mochi validate --runtime
docker compose up -d
curl --fail http://localhost:8080/readyz

# Register an encrypted LLM API key for each repository:
docker compose exec -T mochi mochi credentials set owner/repo gemini --api-key-stdin
```

Compose exposes port 8080 on localhost. GitHub webhooks need a public HTTPS
endpoint; the guide also covers the supplied [Render Blueprint](render.yaml).

For CLI use, install the Go version required by [go.mod](go.mod) or later, then
build from the repository root:

```sh
go build -o mochi ./cmd/mochi
./mochi validate .mochi/workflows/pr-review.yaml .mochi/workflows/pr-review-small.yaml
./mochi run --repo . --base HEAD~1 --head HEAD --dry-run
```

Validation and local dry runs need no LLM or Daytona credentials. Actual reviews
require per-repository LLM credentials (for server mode) or provider env vars
(for `mochi run`) and a prepared snapshot; see [usage](docs/usage.md).

## This repository's workflows

| Workflow | Target branch | Specialists | Daytona snapshot |
| --- | --- | --- | --- |
| [pr-review](.mochi/workflows/pr-review.yaml) | `main` | Correctness, security, performance/architecture, maintainability, and tests | `mochi-review-go` |
| [pr-review-small](.mochi/workflows/pr-review-small.yaml) | `develop` | Correctness | `mochi-review` |

Both workflows use Gemini with built-in embedded prompts under
[`configs/prompts/`](configs/prompts/) and a final synthesizer. Each installed
repository defines its own `.mochi/workflows/*.yaml` on its base branch, allowing
different branch rules (e.g. `feature/* -> develop` vs `develop -> main`), models,
and comment commands. With the GitHub App installed and subscribed to
`pull_request` and `issue_comment`, workflows run on open, non-draft PRs or via
comments such as `/mochi review` and `/mochi review-small`.

## Documentation

- [Usage](docs/usage.md): CLI commands (`serve`, `credentials`, `run`, `inspect`, `validate`) and HTTP endpoints.
- [Configuration](docs/configuration.md): per-repo workflows, branch triggers, built-in/custom prompts, and sinks.
- [Deployment and operations](docs/deployment.md): GitHub App setup, snapshots, hosting, per-repo credentials, and backups.
- [Contributing](CONTRIBUTING.md): development setup, checks, and pull requests.
- [Architecture](docs/architecture.md): execution flow, package responsibilities, and trust boundaries.
