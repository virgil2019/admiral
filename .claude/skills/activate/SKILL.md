---
name: activate
description: Activate a staged admiral task — add the pickup label to a parent issue's sub-issues so admiral's discoverer starts shipping them. This is the explicit execution trigger that complements decompose (which only stages, never labels). ALWAYS confirms the exact project and parent issue with the user before applying any label, to prevent activating the wrong task. Invoke when the user says "/activate", "激活任务", "触发执行", "开始执行 <issue>", "让 admiral 开始做 <task>", or wants to kick off autonomous shipping for an already-decomposed task. Requires the Linear MCP.
---

# Activate

Pull the trigger on a staged task: add the pickup label to a parent issue's
sub-issues so admiral's discoverer picks them up and starts shipping.

This is the deliberate, human-gated counterpart to `decompose`. Decompose
creates the issue structure but never labels anything; `activate` is the
explicit "go" — and because labeling kicks off autonomous work, it MUST NOT
act on the wrong project or issue. Confirmation is mandatory.

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

## Step 3 — CONFIRM with the user (mandatory gate)

This is the safety gate. Present a clear summary and require an explicit
confirmation before touching anything. Show:

- **Project**: name (and id)
- **Parent task**: identifier + title + URL
- **Pickup label** that will be applied: e.g. `agent-ready`
- **Sub-issues that will be labeled**, and for each, what happens next:
  - **will start now** — no unresolved blockers
  - **will wait** — has a `blockedBy` that isn't completed yet (admiral parks
    it BLOCKED until its blockers finish)
- Any sub-issue that **already** has the label (will be skipped / no-op)

Example confirmation prompt:

```
About to ACTIVATE this task — admiral will start shipping PRs autonomously.

Project: snipd (proj-abc123)
Task:    [GEO-1] snipd — minimal text-snippet HTTP API
Label:   agent-ready  (discoverer.require_label)

Sub-issues to label (4):
  [GEO-2] Foundation: model + Store + router   → starts now
  [GEO-3] POST /snippets handler               → waits (blocked by GEO-2)
  [GEO-4] GET /snippets handler                → waits (blocked by GEO-2)
  [GEO-5] TTL sweeper                          → waits (blocked by GEO-2)

Proceed? This kicks off autonomous shipping.
```

Wait for an explicit "yes" (or equivalent). If the user wants only a subset
(e.g. activate the foundation first), honor that — label only the issues they
name.

Do NOT apply any label before this confirmation. If anything about the
project or parent looks off (wrong project, unexpected sub-issues), surface it
and let the user correct course rather than proceeding.

## Step 4 — Apply the pickup label

After explicit confirmation, add the pickup label to each chosen sub-issue:

```
linear.save_issue(id: <sub-issue identifier>, labels: ["agent-ready"])
```

`labels` is additive in admiral's usage here — you are adding the pickup
label, not replacing the issue's existing labels (confirm the MCP merges
rather than overwrites; if it overwrites, include the issue's existing labels
in the call so none are lost).

Never label the parent issue — it is the task definition, never picked up.

## Step 5 — Report

Tell the user:

- Which sub-issues were labeled (and any skipped because already labeled)
- Which will start shipping now vs which are parked behind blockers
- That admiral's discoverer will pick them up on its next poll tick (not
  necessarily instant — depends on `discoverer.poll_interval`)

## Constraints

- NEVER apply a label without the Step 3 confirmation. Wrong-project / wrong-issue
  activation is the exact failure this skill exists to prevent.
- Never label the parent issue.
- Resolve the target to exactly one issue before doing anything — never guess
  between multiple matches.
- Activating is additive and idempotent: an already-labeled sub-issue is a
  no-op, safe to re-run.
- Respect partial activation — if the user only wants some sub-issues labeled,
  label only those.
