---
name: activate
description: Activate a staged admiral task — add the pickup label (`agent-ready`) to a parent issue's `agent-task`-tagged sub-issues so admiral's discoverer starts shipping them. Human-only sub-issues (without `agent-task`) are surfaced as skipped, not labeled. This is the explicit execution trigger that complements decompose (which only stages and classifies, never applies the pickup label). ALWAYS confirms the exact project and parent issue with the user before applying any label, to prevent activating the wrong task. Invoke when the user says "/activate", "激活任务", "触发执行", "开始执行 <issue>", "让 admiral 开始做 <task>", or wants to kick off autonomous shipping for an already-decomposed task. Requires the Linear MCP.
---

# Activate

Pull the trigger on a staged task: add the pickup label (`agent-ready`) to a
parent issue's `agent-task`-tagged sub-issues so admiral's discoverer picks
them up and starts shipping.

This is the deliberate, human-gated counterpart to `decompose`. Decompose
creates the issue structure and tags agent-doable sub-issues with
`agent-task`, but never applies the pickup label; `activate` is the explicit
"go" — and because labeling kicks off autonomous work, it MUST NOT act on the
wrong project or issue. Confirmation is mandatory.

## The two-label model

admiral's skill-layer pipeline uses two orthogonal labels (set on sub-issues
only, never on the parent):

- **`agent-task`** (classification, set by `decompose`) — marks a sub-issue
  as something admiral is capable of doing. Stable metadata.
- **`agent-ready`** (pickup trigger, set by THIS skill) — the team's
  configured `discoverer.require_label`. The discoverer ships any issue
  carrying this label.

`activate` only puts `agent-ready` on issues that already carry `agent-task`.
Human-only sub-issues (no `agent-task`) are explicitly skipped and surfaced
to the user in the confirmation prompt — they are real work units, just for a
human, not for admiral.

## Before you start

Confirm the Linear MCP is available. If `linear.save_issue` is not available,
tell the user and stop.

## Step 1 — Identify the target parent issue

Take what the user gave (issue identifier like `GEO-12`, a URL, or a title)
and resolve it to a single Linear issue.

- If the input is ambiguous (matches multiple issues, or is just a vague
  title), use `linear.list_issues` / search and ask the user to pick the
  exact one. Do NOT guess.
- If you cannot resolve it to exactly one issue, stop and ask.

## Step 2 — Gather the full picture

For the resolved parent issue, fetch:

- The **project** it belongs to (name + id).
- The **team** it belongs to.
- Its **sub-issues** (`children`), each with: identifier, title, current
  state, current labels, and any `blockedBy` relations.

If the issue has **no sub-issues**, it is not a decomposed task — stop and
tell the user (activating a label on a childless issue is almost certainly a
mistake; confirm what they actually meant).

Determine the pickup label name: the team's configured
`discoverer.require_label` (default `agent-ready`). If you don't know the
configured value, ask the user to confirm the label name before proceeding.

**Partition the sub-issues by `agent-task`**:

- **agent-task** — sub-issue's labels include `agent-task`. Candidate for the
  pickup label.
- **human-only** — sub-issue does NOT carry `agent-task`. Will be surfaced as
  "skipped: not an agent task" in Step 3 and left untouched in Step 4.

If **every** sub-issue is human-only, stop and tell the user: there is
nothing for admiral to do under this parent. They likely want to either
re-decompose, manually re-classify a sub-issue as agent-task in Linear, or
just track the work as human-only.

If the partition looks wrong to the user (e.g. a sub-issue they expected to
be agent-task isn't tagged), do NOT add the `agent-task` label from inside
activate — surface the mismatch and let them fix it in Linear or re-run
decompose. activate's responsibility is the pickup trigger, not
re-classification.

## Step 3 — CONFIRM with the user (mandatory gate)

This is the safety gate. Present a clear summary and require an explicit
confirmation before touching anything. Show:

- **Project**: name (and id)
- **Parent task**: identifier + title + URL
- **Pickup label** that will be applied: e.g. `agent-ready`
- **agent-task sub-issues to label**, and for each, what happens next:
  - **will start now** — no unresolved blockers
  - **will wait** — has a `blockedBy` that isn't completed yet (admiral parks
    it BLOCKED until its blockers finish; the blocker can be agent-task or
    human-only — admiral doesn't care, it just waits for completed state)
  - **already labeled** — sub-issue already carries the pickup label (no-op)
- **human-only sub-issues skipped** — sub-issues without `agent-task`,
  listed for visibility so the user can see the whole picture (these will
  NOT be labeled; they are tracked work for a human)

Example confirmation prompt:

```
About to ACTIVATE this task — admiral will start shipping PRs autonomously.

Project: snipd (proj-abc123)
Task:    [GEO-1] snipd — minimal text-snippet HTTP API
Label:   agent-ready  (discoverer.require_label — only applied to agent-task issues)

agent-task sub-issues to label (4):
  [GEO-2] Foundation: model + Store + router   → starts now
  [GEO-3] POST /snippets handler               → waits (blocked by GEO-2)
  [GEO-4] GET /snippets handler                → waits (blocked by GEO-2)
  [GEO-5] TTL sweeper                          → waits (blocked by GEO-6 — human)

human-only sub-issues (skipped, no agent-ready):
  [GEO-6] API contract review with stakeholder team

Proceed? This kicks off autonomous shipping for the 4 agent-task issues.
GEO-5 will sit BLOCKED until GEO-6 is closed by a human.
```

Wait for an explicit "yes" (or equivalent). If the user wants only a subset
of the agent-task issues (e.g. activate the foundation first), honor that —
label only the agent-task issues they name. Never label a human-only issue
with `agent-ready` from inside this skill, even if the user names it: tell
them they likely meant to first re-classify (add `agent-task`) in Linear or
re-run decompose.

Do NOT apply any label before this confirmation. If anything about the
project or parent looks off (wrong project, unexpected sub-issues, surprising
classification), surface it and let the user correct course rather than
proceeding.

## Step 4 — Apply the pickup label (agent-task issues only)

After explicit confirmation, add `agent-ready` to each chosen **agent-task**
sub-issue. Skip human-only issues entirely.

```
linear.save_issue(id: <sub-issue identifier>, labels: ["agent-ready"])
```

`labels` is additive in admiral's usage here — you are adding the pickup
label, not replacing the issue's existing labels (`agent-task` must stay).
Confirm the MCP merges rather than overwrites; if it overwrites, include the
issue's existing labels (including `agent-task`) in the call so none are
lost.

Never label the parent issue — it is the task definition, never picked up.
Never put `agent-ready` on a human-only sub-issue.

## Step 5 — Report

Tell the user:

- Which agent-task sub-issues were labeled (and any skipped because already
  labeled)
- Which will start shipping now vs which are parked behind blockers (calling
  out any blocker that is human-only, so the user knows a human action is on
  the critical path)
- Which human-only sub-issues were skipped (and why — no `agent-task`)
- That admiral's discoverer will pick them up on its next poll tick (not
  necessarily instant — depends on `discoverer.poll_interval`)

## Constraints

- NEVER apply a label without the Step 3 confirmation. Wrong-project / wrong-issue
  activation is the exact failure this skill exists to prevent.
- Never label the parent issue.
- Never apply `agent-ready` to a sub-issue that lacks `agent-task` — even if
  the user names it. Re-classification belongs in Linear or `decompose`, not
  in `activate`.
- Resolve the target to exactly one issue before doing anything — never guess
  between multiple matches.
- Activating is additive and idempotent: an already-labeled sub-issue is a
  no-op, safe to re-run.
- Respect partial activation — if the user only wants some agent-task
  sub-issues labeled, label only those.
