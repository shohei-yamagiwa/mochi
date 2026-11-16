# Configuration

Server workflows are YAML files under [`configs/workflows/`](../configs/workflows/).
Prompts are Go `text/template` files under [`configs/prompts/`](../configs/prompts/).
Start by copying a bundled workflow, then validate it with `mochi validate`.
The field definitions live in
[`internal/domain/workflow.go`](../internal/domain/workflow.go); unknown YAML
fields are rejected.

## Workflow settings

| Section | Controls |
| --- | --- |
| `version`, `name`, `description` | Workflow identity and description |
| `input` | Adapter and GitHub trigger filters |
| `repo_overrides` | Whether repository configuration, prompts, or images may override server settings |
| `sandbox` | Daytona snapshot or image, forking, and setup timeout |
| `agents` | Specialist roles, prompts, models, tools, and budgets |
| `synthesizer` | Final synthesis prompt, model, and budgets |
| `output.sinks` | Artifact destinations and presentation settings |

The [full preset](../configs/workflows/pr-review.yaml) defines correctness,
security, performance/architecture, maintainability, and test roles. The
[small preset](../configs/workflows/pr-review-small.yaml) defines correctness only.
Both use a synthesizer and independent specialist sandboxes with
`fork_from_base: false`. Customize `sandbox.default_snapshot` for the language
runtimes needed by your target repositories; see [snapshot setup](deployment.md#prepare-daytona-snapshots).

GitHub triggers include an event list, `comment_command`, `ignore_drafts`, and
optional `allowed_comment_associations`. The default command associations are
`OWNER`, `MEMBER`, and `COLLABORATOR`. Trigger filters are evaluated before
repository overrides are loaded. An empty event list restores default events.

## LLM providers

Each specialist and the synthesizer has its own `llm` settings. The executable
registers providers when the corresponding environment variables are set:

| YAML provider | Environment |
| --- | --- |
| `gemini` | `GEMINI_API_KEY`, falling back to `GOOGLE_API_KEY` |
| `openai` | `OPENAI_API_KEY` |
| `anthropic` | `ANTHROPIC_API_KEY` |
| `openai_compat` | `OPENAI_COMPAT_BASE_URL`; optional `OPENAI_COMPAT_API_KEY`, falling back to `OPENAI_API_KEY` |

Set `model` to an identifier available to your account. Optional settings include
`temperature`, `max_output_tokens`, `reasoning_effort`, and `base_url`. Supported
reasoning values are `minimal`, `low`, `medium`, `high`, `xhigh`, and `max`;
providers map these to their own supported levels. When changing providers or
models, update every relevant specialist and the synthesizer, then verify actual
API access. Local runtime validation does not check model availability.

## Tools, prompts, and budgets

Built-in tool names are `read_file`, `write_file`, `list_dir`, `search_code`,
`git_diff`, `exec_command`, and `submit_output`. Each specialist sees only the
tools in its `tools` list. `exec_command` can modify the sandbox through shell
commands even when `write_file` is absent. The synthesizer uses structured model
output and has no execution tools.

Prompts receive secret-free `.Task` and `.Workflow` contexts and the current
`.Role` for specialists. Synthesis also receives `.AgentSummaries` and `.Coverage`.
Helpers include `shortSHA`, `truncate`, `join`, `toJSON`, `indent`, and `default`.
See [`internal/config/prompt.go`](../internal/config/prompt.go) for exact fields
and helper arguments. Server prompt paths are relative to `--base-dir` or the
automatically detected project root.

Budgets set `max_turns`, cumulative `max_tokens`, `command_timeout`, and
`run_timeout`. Durations use strings such as `60s` or `5m`. Token budgets count
input and output across calls, including cached input tokens; accounting occurs
after calls. Specialists can receive a final submission-only turn near their
budget. These are execution limits, not exact monetary caps.

## Repository overrides

When enabled by the server workflow, Mochi reads `.mochi/config.yaml` and custom
prompts from the PR base branch. The full preset enables overrides and custom
prompts, while disallowing custom images; the small preset disables overrides.
For example, a repository can lower a role's budget and adjust an existing sink:

```yaml
agents:
  - role: correctness
    budgets:
      max_turns: 8
      run_timeout: 3m
output:
  sinks:
    - type: github_pr_review
      min_severity: medium
      min_confidence: 0.8
```

Overrides may disable existing roles, reduce tool allowlists, tune supported
inference settings, and adjust output filtering. Budget and output-token requests
are clamped to server limits. Keep at least one specialist enabled.

Custom `prompt_file` paths must stay under `.mochi/` and require
`allow_custom_prompts`. Sandbox changes require `allow_custom_images`. Overrides
cannot change trigger rules, providers, models, API base URLs, sink destinations,
or verdict mode, and cannot add roles, tools, or sinks. The allowlist and merging
rules are implemented in [`internal/config/loader.go`](../internal/config/loader.go).

## Output sinks

| Type | Destination and settings |
| --- | --- |
| `github_pr_review` | Check Run and PR review; `post_inline_comments`, `include_coverage_summary`, `min_severity`, `min_confidence`, `verdict_mode` |
| `console` | Human-readable report on stdout; optional coverage summary |
| `json_file` | JSON artifact at `options.path` |
| `webhook` | JSON envelope sent to `url`; optional HMAC signing secret in `options.secret` |

For GitHub output, `comment_only` posts comments without enforcing the artifact's
verdict; `enforce` allows approval or change-request reviews. Severity thresholds
are `nit`, `low`, `medium`, `high`, or `critical`; confidence ranges from 0 to 1.
The bundled workflows use `comment_only`, minimum severity `low`, and minimum
confidence `0.7`. CLI runs suppress GitHub delivery unless `--post-github` is set.

Keep server workflow files containing webhook secrets private. Prompt contexts
omit sink URLs and options, but resolved workflow configuration is persisted for
inspection, so protect database and API access as well.
