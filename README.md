# mochi

Mochi is a Go server and CLI for agent workflows. Its bundled workflows review
GitHub pull requests with LLM agents, execute tools in isolated Daytona
sandboxes, and store reports, execution traces, and diff coverage in SQLite.

## Get started

Follow the [deployment guide](docs/deployment.md) to configure a GitHub App,
API credentials, and a Daytona snapshot. Then start the server with Docker Compose:

```sh
cp .env.example .env
chmod 600 .env
# Replace the placeholders in .env before continuing.
docker compose build
docker compose run --rm mochi validate --runtime configs/workflows/pr-review.yaml
docker compose up -d
curl --fail http://localhost:8080/readyz
```

Compose exposes port 8080 on localhost. GitHub webhooks need a public HTTPS
endpoint; the guide also covers the supplied [Render Blueprint](render.yaml).

For CLI use, install the Go version required by [go.mod](go.mod) or later, then
build from the repository root:

```sh
go build -o mochi ./cmd/mochi
./mochi validate configs/workflows/pr-review.yaml configs/workflows/pr-review-small.yaml
./mochi run --repo . --base HEAD~1 --head HEAD --dry-run
```

Validation and local dry runs need no LLM or Daytona credentials. Actual reviews
require provider credentials and a prepared snapshot; see [usage](docs/usage.md).

## Review workflows

| Preset | Specialists | Daytona snapshot |
| --- | --- | --- |
| [Full](configs/workflows/pr-review.yaml), the default | Correctness, security, performance/architecture, and tests | `mochi-review-go` |
| [Small](configs/workflows/pr-review-small.yaml) | Correctness | `mochi-review` |

Both presets use Gemini and a final synthesizer that combines specialist results.
Each specialist creates its own sandbox; the synthesizer uses no sandbox.
The container runs one workflow at a time, with specialists running in parallel.

With the GitHub App installed and subscribed to `pull_request` and `issue_comment`,
both presets review open, non-draft PRs when opened, updated, reopened, or marked
ready. Owners, members, and collaborators can rerun a review with `/mochi review`.
The presets publish comments and Check Runs without enforcing the review verdict.

## Documentation

- [Usage](docs/usage.md): CLI commands, HTTP endpoints, and review results.
- [Configuration](docs/configuration.md): workflows, providers, prompts, and repository overrides.
- [Deployment and operations](docs/deployment.md): GitHub App setup, snapshots, hosting, and backups.
- [Contributing](CONTRIBUTING.md): development setup, checks, and pull requests.
- [Architecture](docs/architecture.md): execution flow, package responsibilities, and extension points.
