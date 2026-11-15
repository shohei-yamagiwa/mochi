# mochi
Mochi is a workflow-agnostic agent orchestration server.

Mochi reviews GitHub pull requests with LLM agents running tools in isolated
Daytona sandboxes, and stores runs, traces, and diff coverage in SQLite.

## Deploy as a GitHub App

See **[デプロイ手順とホスティング比較](docs/deployment.md)** for GitHub App permissions,
secrets, Daytona snapshot setup, Render deployment, verification, and recovery.

- [Dockerfile](Dockerfile) and [Compose](compose.yaml) package the existing Go application.
- [Render Blueprint](render.yaml) runs one paid instance with a persistent disk.
- [Small preset](configs/workflows/pr-review-small.yaml): one specialist + synthesizer,
  Gemini only, one concurrent workflow. Subscribe to `pull_request` and
  `issue_comment` events for automatic reviews when a non-draft PR is opened,
  updated, reopened, or marked ready. Owners/members/collaborators can also
  invoke `/mochi review` to rerun a review.
- [Full preset](configs/workflows/pr-review.yaml): four specialists using OpenAI,
  Anthropic, and Gemini. It needs more API keys and a suitable Daytona snapshot.

```sh
cp .env.example .env
chmod 600 .env
# Fill .env and create the Daytona snapshot as described in the deployment guide.
docker compose build
docker compose run --rm mochi validate --runtime configs/workflows/pr-review-small.yaml
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
