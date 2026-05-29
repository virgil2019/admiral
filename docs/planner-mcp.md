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
  match the user's original intent? Currently completed by the host
  agent reading `feature_get_materials`; automatic follow-up issue
  creation is not yet implemented (see Limitations).

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

| Variable           | Required | Purpose                                              |
| ------------------ | -------- | ---------------------------------------------------- |
| `ADMIRAL_DB_PATH`  | yes      | Absolute path to the admiral SQLite file.            |
| `ADMIRAL_GH_TOKEN` | optional | GitHub PAT; needed for `pr_get_materials` + `pr_verify_submit`. Without it those tools return a clear "set ADMIRAL_GH_TOKEN" error and other tools still work. |

Linear OAuth is read from the admiral database, not env — no separate
config needed if admiral itself is already authenticated.

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
- **No automatic L2 follow-up issue creation.** When L2 acceptance
  finds a gap, the host agent has to ask the user to create a Linear
  issue manually, then call `issue_set_acceptance` to register
  criteria. A `feature_followup_submit` tool that creates issues via
  the Linear API is planned but not yet implemented (requires adding
  an `IssueCreate` mutation to `internal/linear`).
- **Single-database.** The server reads `ADMIRAL_DB_PATH`; all
  features go in one DB. Multi-tenant separation is out of scope.

## Logging

All logs go to **stderr**. stdout is reserved for the JSON-RPC
protocol stream — anything written there will corrupt the host agent's
parse loop. If you need to debug, look at the host agent's MCP server
log capture (Claude Code: `~/Library/Logs/Claude/mcp-server-admiral-planner.log`
or equivalent for your platform).
