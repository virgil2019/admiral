---
name: decompose
description: Decompose a task into a Linear issue hierarchy ready for the admiral autonomous loop — one parent issue holding the PRD, plus orthogonal sub-issues (foundation-first, dependency-ordered) each with acceptance criteria, wired with blocking relations and labelled agent-ready last so the discoverer picks them up only after setup is complete. Invoke when the user says "/decompose", "decompose this", "拆任务", "拆分到 Linear", "break this down into issues", or wants to turn a PRD/design doc into admiral-ready Linear sub-issues. Requires the Linear MCP.
---

# Decompose

Break a task into a Linear issue hierarchy ready for the admiral autonomous loop.

## Before you start

Confirm you have Linear MCP available. If `linear.save_issue` is not available, tell the user and stop.

## Step 1 — Confirm PRD is ready

Ask the user to confirm the requirements doc (design doc, PRD, issue description, or equivalent) is ready and accessible. Do not proceed until confirmed.

If the user says the doc is not ready, stop and tell them to prepare it first.

## Step 2 — Clarify if needed

Read the PRD. If anything is ambiguous (scope, acceptance criteria, non-obvious decisions), use the Clarify tool to ask the user to clarify before decomposing.

You may resolve ambiguities yourself if you can infer the intent from context — only ask when genuinely unclear.

## Step 3 — Analyze the requirements

Extract from the PRD:

- **Core goal**: what the task is trying to achieve
- **Functional pieces**: distinct features or capabilities that can be built and shipped independently
- **Dependencies**: which pieces must be done before others (framework before plugins, data models before business logic, etc.)
- **Orthogonality**: pieces that can run in parallel without PR conflicts — keep them independent
- **Acceptance criteria**: concrete, verifiable conditions for each piece

## The label gates everything — set it LAST

This is the load-bearing invariant of the whole skill. admiral's discoverer
picks up any sub-issue that carries the pickup label (the team's configured
`discoverer.require_label`, default `agent-ready`) in a pickable state. The
moment a sub-issue gets that label, it can be shipped on the next discoverer
tick.

So every other setup step — parent attachment, blocking relations,
acceptance criteria — MUST be in place before the label is added. The phases
below are ordered to guarantee this: labels go on only in the final phase,
after all IDs exist and all `blockedBy` relations are set.

Use `linear.save_issue` for everything (create when `id` is omitted, update
when `id` is passed). It supports `parentId`, `labels`, `blockedBy`,
`blocks`, and `state` directly.

## Step 4 — Create the parent issue

```
linear.save_issue(
  team:        <ask the user which Linear team, or use the one in context>,
  project:     <the project mapped to the target repo in admiral's config, if known>,
  title:       <task name>,
  description: <full PRD — the ground-truth requirement admiral's verify loop judges against>,
)
```

Do NOT label the parent. The parent is the task definition, never a work
unit — the discoverer only ever picks up its sub-issues.

Record the returned parent identifier.

## Step 5 — Create sub-issues (no label yet)

For each functional piece from Step 3, create a sub-issue WITHOUT the pickup
label:

```
linear.save_issue(
  team:        <same team>,
  project:     <same project>,
  parentId:    <parent identifier>,
  title:       <sub-task name>,
  description: <acceptance criteria — concrete, verifiable conditions this sub-issue's PR must meet>,
  state:       <a pickable state — see note>,
)
```

Record every sub-issue identifier. Do this for ALL sub-issues before moving
on, so their IDs exist for the blocking step.

**Decompose so that:**

1. **Foundation is its own sub-issue, depended on by the rest** — frameworks,
   data models, global variables, shared interfaces, base types. This ships
   first and everything else blocks on it.
2. **Parallel pieces are orthogonal** — no shared mutable state, no
   overlapping file edits, so their PRs never conflict. If two pieces would
   touch the same file, either merge them into one sub-issue or split along
   an interface so each owns its own files.
3. **At most ~10 sub-issues.** If the task needs more, it should be multiple
   parent tasks — tell the user.

**Note on state:** the sub-issue must land in a state whose type is in the
discoverer's `state_types` (default `backlog` / `unstarted`) or it won't be
pickable even with the label. New issues usually default to a backlog state;
set `state` explicitly (e.g. "Backlog" or "Todo") if unsure.

## Step 6 — Set blocking relations

Now that every sub-issue ID exists, wire dependencies. For each dependent
sub-issue B that needs prerequisite A done first:

```
linear.save_issue(id: <B identifier>, blockedBy: [<A identifier>, ...])
```

`blockedBy` is append-only and accepts multiple blockers. Set every
dependency edge here. admiral's BlockerWatcher reads these `blocked_by`
relations: a sub-issue with an unresolved blocker is parked BLOCKED and
auto-resumes once its blockers reach a completed state — so foundation ships
first, dependents wait, naturally.

## Step 7 — Add the pickup label (final step, opens the gate)

Only now, with parents attached and all blocking relations set, label every
sub-issue so the discoverer starts picking them up:

```
linear.save_issue(id: <sub-issue identifier>, labels: ["agent-ready"])
```

Use the team's configured `discoverer.require_label` value (default
`agent-ready`). Adding the label any earlier opens a race where the
discoverer grabs an issue before its blockers exist — that is exactly the
window this ordering closes.

## Step 8 — Summary

Report to the user:

- Parent issue URL
- Sub-issue URLs with acceptance criteria summary
- Dependency chain (which sub-issue must complete before which)
- Confirmation that blocking relations were set and the pickup label applied

Example summary:

```
Parent: [GEO-1] Build login feature
  └─ [GEO-2] Data models + auth interfaces (foundation)
     └─ [GEO-3] Email/password auth (core)
        └─ [GEO-4] OAuth integration (dependent)
     └─ [GEO-5] Session management (parallel with GEO-3)

Blocking relations: set (GEO-3/GEO-5 blocked by GEO-2; GEO-4 blocked by GEO-3)
Labels: agent-ready applied to GEO-2, GEO-3, GEO-4, GEO-5
```

## Constraints

- Parent issue NEVER gets agent-ready label — it is the task definition, not a work unit
- Only sub-issues get agent-ready label, added at the very end
- Sub-issue description = acceptance criteria, not implementation details — this is what admiral's verify loop judges against
- If the user provides vague requirements, ask clarifying questions before decomposing
- Do not create more than ~10 sub-issues — if a task is that large, suggest decomposing into multiple parent tasks
