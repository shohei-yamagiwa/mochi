# mochi
Mochi is a workflow-agnostic agent orchestration server.

Mochi reviews GitHub pull requests with LLM agents running tools in isolated
Daytona sandboxes, and stores runs, traces, and diff coverage in SQLite.

## Deploy as a GitHub App

See **[デプロイ手順とホスティング比較](docs/deployment.md)** for GitHub App permissions,
secrets, Daytona snapshot setup, Render deployment, verification, and recovery.

- [Dockerfile](Dockerfile) and [Compose](compose.yaml) package the existing Go application.
- [Render Blueprint](render.yaml) runs one paid instance with a persistent disk.
- [Full preset](configs/workflows/pr-review.yaml), the deployment default: four Gemini
  specialists (correctness, security, performance/architecture, and tests) plus one
  Gemini synthesizer. Each specialist directly creates its own Daytona container
  sandbox from `mochi-review-go`; the synthesizer needs no sandbox.
- [Small preset](configs/workflows/pr-review-small.yaml): one specialist + synthesizer,
  Gemini only, using the existing `mochi-review` snapshot. Keep it for rollback.
- Both presets support automatic reviews. Subscribe to `pull_request` and
  `issue_comment` events for automatic reviews when a non-draft PR is opened,
  updated, reopened, or marked ready. Owners/members/collaborators can also
  invoke `/mochi review` to rerun a review.
- One workflow runs at a time; the full preset can use four sandboxes concurrently.
  See the [migration runbook](docs/multi-agent-rollout.md) before changing an existing
  deployment. Both presets use the existing `GEMINI_API_KEY`.

```sh
cp .env.example .env
chmod 600 .env
# Fill .env and create the Daytona snapshot as described in the deployment guide.
docker compose build
docker compose run --rm mochi validate --runtime configs/workflows/pr-review.yaml
docker compose up -d
curl --fail http://localhost:8080/readyz
```

For local development with Go 1.27.3 or later:

```sh
go test ./...
go run ./cmd/mochi validate configs/workflows/pr-review.yaml configs/workflows/pr-review-small.yaml
```

`validate --runtime` checks local credentials/configuration without making external
API calls. Remote API access, model availability, and a real PR review still need
verification after deployment. The [architecture document](docs/architecture.md)
describes the broader system design.
