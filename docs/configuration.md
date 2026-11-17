# Configuration

In server mode, Mochi discovers workflow YAML files directly from each
installed repository's `.mochi/workflows/*.yaml` (or `*.yml`) directory on the
PR base branch. Built-in prompt templates under [`configs/prompts/`](../configs/prompts/)
are embedded in the Mochi binary, while repositories can optionally place custom
prompts under `.mochi/`.

Validate workflow files locally with `mochi validate`. The field definitions
live in [`internal/domain/workflow.go`](../internal/domain/workflow.go);
unknown YAML fields are rejected.

## Repository workflows and examples

This repository maintains two example workflows under `.mochi/workflows/`:

- [`.mochi/workflows/pr-review.yaml`](../.mochi/workflows/pr-review.yaml):
  five specialists (correctness, security, performance/architecture,
  maintainability, and test-runner) triggered on PRs targeting `main`
  (`base_branches: ["main"]`) or `/mochi review`.
- [`.mochi/workflows/pr-review-small.yaml`](../.mochi/workflows/pr-review-small.yaml):
  one correctness specialist triggered on PRs targeting `develop`
  (`base_branches: ["develop"]`) or `/mochi review-small`.

| Section | Controls |
| --- | --- |
| `version`, `name`, `description` | Workflow identity and description |
| `input` | Adapter and GitHub trigger filters (`events`, `base_branches`, `head_branches`, `comment_command`) |
| `repo_overrides` | Optional `.mochi/config.yaml` overrides when running a static server workflow |
| `sandbox` | Daytona snapshot or image, forking, and setup timeout |
| `agents` | Specialist roles, prompts, models, tools, and budgets |
| `synthesizer` | Final synthesis prompt, model, and budgets |
| `output.sinks` | Artifact destinations and presentation settings |

## GitHub triggers and branch filters

GitHub triggers support:

- `events`: PR actions (`opened`, `synchronize`, `reopened`, `ready_for_review`,
  etc.). Omitting `events` defaults to `["opened", "synchronize", "reopened",
  "ready_for_review"]`. Setting `events: []` explicitly disables automatic PR
  event triggering (useful for comment-only workflows).
- `base_branches`: optional list of target branch patterns (globs supporting `*`,
  `?`, and `**`, e.g. `["main"]`, `["release/**"]`). Empty matches all base
  branches.
- `head_branches`: optional list of source branch patterns using the same glob
  syntax (e.g. `["develop"]`, `["feature/**"]`). Empty matches all head
  branches.
- `comment_command`: slash command in PR comments (e.g. `"/mochi review"`).
  Comment commands still respect `base_branches` and `head_branches` filters.
- `allowed_comment_associations`: defaults to `["OWNER", "MEMBER", "COLLABORATOR"]`.
- `ignore_drafts`: skips draft PRs when true.

Example routing `feature/* -> develop` PRs to a lightweight review and
`develop -> main` PRs to a full review:

```yaml
# .mochi/workflows/pr-review-small.yaml
input:
  adapter: github_pr
  triggers:
    events: ["opened", "synchronize", "reopened", "ready_for_review"]
    base_branches: ["develop"]
    head_branches: ["feature/**"]
    comment_command: "/mochi review-small"
    ignore_drafts: true
```

```yaml
# .mochi/workflows/pr-review.yaml
input:
  adapter: github_pr
  triggers:
    events: ["opened", "synchronize", "reopened", "ready_for_review"]
    base_branches: ["main"]
    head_branches: ["develop"]
    comment_command: "/mochi review"
    ignore_drafts: true
```

Multiple matching workflows in `.mochi/workflows/` run independently on the same
PR, each creating its own Check Run (`Mochi Code Review (<workflow_name>)`) and
run record (`DedupKey` qualified by workflow name).

## LLM providers and credentials

Each specialist and the synthesizer defines its own `llm` settings (`provider`,
`model`, optional `temperature`, `max_output_tokens`, `reasoning_effort`, and
`base_url`). Supported providers are `gemini`, `openai`, `anthropic`, and
`openai_compat`. Supported `reasoning_effort` values are `minimal`, `low`,
`medium`, `high`, `xhigh`, and `max`.

- **In server mode (`mochi serve`)**: API keys are resolved **strictly per
  repository** from encrypted SQLite credentials managed via
  [`mochi credentials`](usage.md#manage-per-repository-credentials) or the HTTP
  API (`/api/v1/repos/{owner}/{repo}/credentials/{provider}`). Server environment
  variables (`GEMINI_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`) are never
  used as fallbacks; missing repository credentials fail the run with an
  explicit error.
- **In local CLI mode (`mochi run`)**: providers are read from environment
  variables (`GEMINI_API_KEY` / `GOOGLE_API_KEY`, `OPENAI_API_KEY`,
  `ANTHROPIC_API_KEY`, `OPENAI_COMPAT_BASE_URL` / `OPENAI_COMPAT_API_KEY`).

## Tools, prompts, and budgets

Built-in tool names are `read_file`, `write_file`, `list_dir`, `search_code`,
`git_diff`, `exec_command`, and `submit_output`. Each specialist sees only the
tools in its `tools` list. `exec_command` can modify the sandbox through shell
commands even when `write_file` is absent. The synthesizer uses structured model
output and has no execution tools.

Prompts are Go `text/template` files receiving secret-free `.Task` and
`.Workflow` contexts and `.Role` for specialists (plus `.AgentSummaries` and
`.Coverage` for the synthesizer). Helpers include `shortSHA`, `truncate`, `join`,
`toJSON`, `indent`, and `default`; see
[`internal/config/prompt.go`](../internal/config/prompt.go).

In repository workflows (`.mochi/workflows/*.yaml`), `prompt_file` may reference:

- **Built-in embedded prompts** (e.g. `configs/prompts/correctness.md`): bundled
  directly in the Mochi binary via [`configs/embed.go`](../configs/embed.go), so
  target repositories do not need to copy prompt files.
- **Repository-local prompts** under `.mochi/` (e.g. `.mochi/prompts/custom.md`):
  automatically fetched from the PR base branch commit (`BaseSHA`).

Budgets set `run_timeout` using duration strings such as `60s` or `15m`.

## Output sinks

| Type | Destination and settings |
| --- | --- |
| `github_pr_review` | Check Run and PR review; `post_inline_comments`, `include_coverage_summary`, `min_severity`, `min_confidence` |
| `console` | Human-readable report on stdout; optional coverage summary |
| `json_file` | JSON artifact at `options.path` |
| `webhook` | JSON envelope sent to `url`; optional HMAC signing secret in `options.secret` |

For GitHub output, the review event and Check Run conclusion enforce the artifact's
verdict (`APPROVE` to `success`, `REQUEST_CHANGES` to `failure`, and `COMMENT` to
`neutral`, falling back to a `COMMENT` review if GitHub returns HTTP 422). When
the default Check Run name is used, Mochi qualifies it as
`Mochi Code Review (<workflow_name>)` so multiple workflows on a single PR do not
overwrite each other's Check Runs. Severity thresholds are `nit`, `low`,
`medium`, `high`, or `critical`; confidence ranges from `0` to `1`. CLI runs
suppress GitHub delivery unless `--post-github` is set.

## Static server mode and repository overrides (optional)

If `--workflow` or `MOCHI_WORKFLOW` is explicitly passed to `mochi serve`, Mochi
switches to static workflow mode instead of per-repository `.mochi/workflows/`
discovery. In that mode, `repo_overrides` can still allow `.mochi/config.yaml`
on the PR base branch to adjust existing roles, tools, models, budgets, or sink
thresholds; see [`internal/config/loader.go`](../internal/config/loader.go).
