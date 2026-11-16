# Contributing

## Development setup

Install Git and the Go version required by [go.mod](go.mod) or later. Docker is
needed to check the server and sandbox images. From a clone of this repository:

```sh
go mod download
go build -o mochi ./cmd/mochi
go run ./cmd/mochi validate configs/workflows/pr-review.yaml configs/workflows/pr-review-small.yaml
```

Unit tests and plain workflow validation do not require GitHub, LLM, or Daytona
credentials. For live reviews, configure credentials and snapshots as described
in [deployment](docs/deployment.md). The standalone binary does not load `.env`.

## Project layout

- `cmd/mochi/`: CLI commands, environment configuration, and runtime wiring.
- `internal/`: adapters, orchestration, providers, tools, persistence, and output.
- `configs/`: workflow presets and prompt templates used at runtime.
- `deploy/`: container entrypoint and Daytona snapshot Dockerfiles.
- `docs/`: usage, configuration, deployment, and architecture guides.

See [architecture](docs/architecture.md) for execution flow and extension points.
Tests live alongside the packages they exercise and use fakes or local servers
for external dependencies.

## Check changes

Format changed Go files with `gofmt`. Before submitting code changes, run the
checks used by [CI](.github/workflows/ci.yml):

```sh
go vet ./...
go test -race ./...
go run ./cmd/mochi validate configs/workflows/pr-review.yaml configs/workflows/pr-review-small.yaml
```

For packaging changes, also check the server image and packaged workflow files:

```sh
docker build -t mochi:dev .
docker run --rm mochi:dev validate configs/workflows/pr-review.yaml configs/workflows/pr-review-small.yaml
docker build -f deploy/sandbox/go.Dockerfile -t mochi:sandbox-go .
docker run --rm mochi:sandbox-go sh -c 'go version && git --version && rg --version'
```

These checks do not prove that external credentials, model access, or Daytona
snapshots work. Use a test repository for live integration checks; actual reviews
can incur provider charges and publish to GitHub when enabled. Follow the
[deployment verification steps](docs/deployment.md#verify-a-deployment).

## Pull requests and documentation

Keep changes focused and explain the behavior affected and the checks performed.
Use the [pull request template](.github/pull_request_template.md): it asks for a
summary, the changes, and the checks you ran, with optional sections for impact,
follow-ups, and post-merge steps. Write the title as a
[Conventional Commit](https://www.conventionalcommits.org/) such as `feat: ...`
or `docs(prompts): ...`.
Add regression tests when changing behavior, especially around provider requests,
configuration restrictions, cancellation, cleanup, and persisted contracts.

Write documentation in English for users and contributors. Update the relevant
guide when changing commands, configuration, or behavior. Link to source files for
exact schemas and dependency versions instead of duplicating large definitions.
Keep enduring setup and operational guidance; use issues and pull requests for
temporary rollout steps, implementation plans, and progress reports.

For documentation-only changes, check relative links and anchors, verify commands
and defaults against the implementation, and validate any workflow examples.
Markdown files under `configs/prompts/` are runtime templates: changing them
changes review behavior and should receive the corresponding validation.
