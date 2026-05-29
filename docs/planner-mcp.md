# admiral-planner-mcp

A stdio MCP server that a host agent (claude / codex / any MCP-aware
agent) talks to during two phases of work:

1. **Decomposition** — you discuss a feature with the agent; it records
   the original requirements text + per-issue acceptance criteria in
   the planner database.
2. **Verification** — after admiral-autopilot ships PRs for those
   issues, the agent reads the planner's record back, judges each PR
   against the criteria, and submits verdicts to GitHub.

The split keeps judgment in the agent's LLM (which sees the
conversational context) while persistence lives in the planner store
(which survives across sessions). Two-layer acceptance:

- **L1 per-PR** — does this PR satisfy the issue's acceptance criteria?
  Submitted via `gh pr review`.
- **L2 feature-wide** — after all PRs merge, does the whole feature
  match the user's original intent? The host agent reads
  `feature_get_materials`, and when it finds a gap, calls
  `feature_followup_submit` to open a Linear issue with criteria in one
  step.

## Install

The server lives in the same module as admiral and shares the same
SQLite database, so no separate setup is needed beyond:

```bash
go install ./cmd/admiral-planner-mcp
```

This installs the binary at `$(go env GOPATH)/bin/admiral-planner-mcp`.

## Configure your host agent

The server speaks MCP over stdio. Configure your host agent's MCP
client to launch it.

### Claude Code (`~/.claude.json`)

```json
{
  "mcpServers": {
    "admiral-planner": {
      "command": "admiral-planner-mcp",
      "env": {
        "ADMIRAL_DB_PATH": "/absolute/path/to/admiral.db",
        "ADMIRAL_GH_TOKEN": "ghp_xxx_or_$(gh auth token)"
      }
    }
  }
}
```

### Codex / other MCP clients

Use whatever stdio-MCP configuration the client supports. The two env
vars below are the only contract:

| Variable             | Required | Purpose                                              |
| -------------------- | -------- | ---------------------------------------------------- |
| `ADMIRAL_DB_PATH`    | yes      | Absolute path to the admiral SQLite file.            |
| `ADMIRAL_GH_TOKEN`   | optional | GitHub PAT; needed for `pr_get_materials` + `pr_verify_submit`. Without it those tools return a clear "set ADMIRAL_GH_TOKEN" error and other tools still work. |
| `ADMIRAL_CONFIG_PATH`| optional | Path to admiral's `config.yaml`. Read so `feature_followup_submit` labels / states the issues it creates to match the discoverer's pickup gates (see below). Defaults to admiral's standard config location; without a readable config, issues are created unlabelled / un-stated and must be moved manually. |

Linear OAuth is read from the admiral database, not env — no separate
config needed if admiral itself is already authenticated. Without a token,
`feature_followup_submit` returns a clear "not configured" error and the
other tools still work. `ADMIRAL_LINEAR_ENDPOINT` optionally overrides the
GraphQL endpoint (defaults to `https://api.linear.app/graphql`).

### Pickup contract

For admiral to actually ship a planner-created issue, `admiral-discoverer`
must discover it. The discoverer only picks issues that (a) live in a
project it's opted into (`repos.auto_pick_enabled`), (b) carry its
`require_label`, and (c) sit in a workflow state whose type is in
`state_types`. `feature_followup_submit` reads the same `config.yaml`
(`ADMIRAL_CONFIG_PATH`) and stamps the require_label + a matching state
onto each issue it creates, so the two never drift. When a team has several
states of a wanted type, it picks the lowest-position one (e.g. with
`state_types: [backlog, unstarted]` the issue lands in Backlog); the
discoverer's filter is type-only, so any matching state is pickable, but if
the exact landing state matters, set `state_types` to a single narrow type.
It errors loudly if the configured label or a matching state can't be
resolved in the issue's team — better than silently creating an issue
admiral will never pick up. If `ADMIRAL_CONFIG_PATH` is set but unreadable,
the server refuses to start rather than create un-pickable issues.

## Tools

Listed in roughly the order a typical workflow exercises them.

### `feature_start`
Open a new feature bound to a Linear project.

Input: `{name, requirements_text, linear_project_id, source_agent?}`
- `requirements_text` should be **verbatim user input**, not a paraphrase.
  L2 acceptance compares against intent — paraphrasing introduces drift.
- The host agent must create / pick the Linear project beforehand
  (see Limitations).

Returns `{feature_id, linear_project_id}`. UNIQUE collision on
`linear_project_id` surfaces a "already bound to feature X" error so
the agent can switch to `feature_get_materials` on that feature
instead of retrying.

### `issue_set_acceptance`
Record the L1 standard for one Linear issue inside a feature.

Input: `{feature_id, linear_issue_id, acceptance_criteria}`. Idempotent
— recall with new text overwrites the criteria. Without criteria,
`pr_verify_submit` has nothing to judge against.

### `feature_get_materials`
Read back the feature record for L2 acceptance.

Input: `{feature_id}` → `{feature, issues}`. The host agent uses the
original requirements text as ground truth and the issue criteria as
the L1 contract.

### `issue_list_by_feature`
Enumerate a feature's issues with admiral_tasks state + PR URL.

Input: `{feature_id}` → `{feature_id, issues: [{linear_issue_id,
issue_identifier, acceptance_criteria, state, pr_url}]}`. Use to find
which PRs are ready for L1 verification.

### `pr_get_materials`
Pull everything needed to judge one PR.

Input: `{pr_url}` → `{feature_id, feature_name, linear_issue_id,
issue_identifier, acceptance_criteria, branch, diff}`. The `diff` is
fetched live from GitHub via `gh pr diff`.

### `pr_verify_submit`
Submit an L1 verdict; calls `gh pr review`.

Input: `{pr_url, verdict, reasoning, agent?}`
- `verdict`: `approve` | `request_changes` | `needs_rebase`.
  `needs_rebase` is `request_changes` with a rebase-themed body prefix,
  picked up by admiral's `dispatch_review` handler as a signal to spawn
  claude to rebase.
- `reasoning` required for `request_changes` / `needs_rebase` (gh
  enforces a body); optional for `approve`.

Returns `{submitted, verdict, reason?}`. Idempotent: if the latest
recorded verdict matches the new one, the `gh` call is skipped and
`submitted=false` is returned. The audit row in `pr_verifications` is
only written after `gh` succeeds, so a network failure leaves no
ghost record.

### `feature_followup_submit`
Create a Linear issue for an L2 follow-up gap and register its acceptance
criteria, in one call.

Input: `{feature_id, title, description?, acceptance_criteria}` →
`{linear_issue_id, issue_identifier, url?}`. Use after
`feature_get_materials` reveals the shipped PRs don't fully match user
intent. The issue is created in the feature's Linear project (and that
project's team — see Limitations on multi-team projects), stamped with the
discoverer's pickup label + a pickable state (see "Pickup contract") so
admiral ships it automatically; the criteria is recorded so a later
`pr_verify_submit` on the follow-up's PR has a standard to judge against.
Requires a Linear OAuth token in the admiral DB
(the same one admiral itself uses); returns a clear "not configured" error
without one. Unlike the read tools, this needs no `linear_issue_id` —
unlike `issue_set_acceptance`, it creates the issue rather than annotating
an existing one.

### `feature_close`
Mark the feature done after L2 acceptance.

Input: `{feature_id}` → `{closed, reason?}`. Idempotent re-close
returns `closed=false`; unknown id errors out.

## Workflow

```
host agent (claude/codex)                   admiral-planner-mcp
─────────────────────────                   ───────────────────
user: "build login feature"
agent decomposes
  ─ feature_start ───────────────────────►  features row
  ─ (Linear create issue × N via Linear UI/MCP)
  ─ issue_set_acceptance × N ────────────►  feature_issues rows

(time passes; admiral-autopilot opens PRs)

user: "verify login feature"
  ─ issue_list_by_feature ───────────────►  reads feature_issues
                                            JOIN admiral_tasks
  ◄──────────────────────  {issues+state+pr_url}
for each PR:
  ─ pr_get_materials ────────────────────►  reads criteria
                                            + gh pr diff
  ◄──────────────────────  {criteria, diff}
agent judges in LLM
  ─ pr_verify_submit ────────────────────►  gh pr review
                                            + audit row
                                            (idempotent)
L2: agent reads feature_get_materials, judges whole feature
  ─ feature_followup_submit (on a gap) ──►  Linear issueCreate
                                            + feature_issues row
  ─ feature_close (after all merged) ────►  closed_at stamped
```

## Limitations

These are known gaps; they are real but not blockers for the core
loop, which is "decompose → admiral ships → verify".

- **No Linear project creation.** `feature_start` requires an existing
  `linear_project_id`. Create the project in the Linear UI or via the
  Linear MCP server, then pass its UUID.
- **No Linear issue creation.** Similarly, `issue_set_acceptance`
  requires an existing `linear_issue_id`. Create issues in Linear UI
  first; this server only records criteria against existing issues.
- **Follow-up issues land in the project's first team.** When
  `feature_followup_submit` creates an issue, Linear requires a team but
  a feature only records a project. The server resolves the project's
  first team (`teams(first: 1)`). In a single-team deployment this is
  unambiguous; in a project spanning multiple teams, "first" is not
  ordering-guaranteed — pick the team yourself by creating the issue in
  the Linear UI and calling `issue_set_acceptance` instead.
- **Single-database.** The server reads `ADMIRAL_DB_PATH`; all
  features go in one DB. Multi-tenant separation is out of scope.

## Logging

All logs go to **stderr**. stdout is reserved for the JSON-RPC
protocol stream — anything written there will corrupt the host agent's
parse loop. If you need to debug, look at the host agent's MCP server
log capture (Claude Code: `~/Library/Logs/Claude/mcp-server-admiral-planner.log`
or equivalent for your platform).
