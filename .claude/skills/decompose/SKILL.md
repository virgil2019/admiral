---
name: decompose
description: Decompose a task into a Linear issue hierarchy for the admiral autonomous loop — one parent issue holding the PRD, plus orthogonal sub-issues (foundation-first, dependency-ordered) each with acceptance criteria, wired with blocking relations. Creates the issues only; it deliberately does NOT apply the pickup label, so nothing is shipped until the user explicitly activates the task. Invoke when the user says "/decompose", "decompose this", "拆任务", "拆分到 Linear", "break this down into issues", or wants to turn a PRD/design doc into admiral-ready Linear sub-issues. Requires the Linear MCP.
---

# Decompose

Break a task into a Linear issue hierarchy ready for the admiral autonomous loop.

## Before you start

Confirm you have Linear MCP available. If `linear.save_issue` is not available, tell the user and stop.

## Step 1 — Confirm the PRD exists

Ask the user to confirm the requirements doc (design doc, PRD, issue description, or equivalent) is ready and accessible. Do not proceed until confirmed.

If the user says the doc is not ready, stop and tell them to prepare it first.

## Step 2 — Gate on detail: is the PRD decomposable?

The PRD must be detailed enough to decompose well. The test is not length —
it is this single meta-principle:

> **A PRD is detailed enough when every functional piece you would split out
> can be given a concrete, verifiable acceptance criterion — a black-box
> condition a reviewer (or admiral's verify judge) can check a PR against
> without guessing intent.**

Why this matters: the acceptance criteria you write become each sub-issue's
description, and that is the ONLY thing admiral's verify loop judges the
shipped work against. Vague criteria → the judge cannot tell done from
not-done → the loop cannot converge. A thin PRD does not fail loudly; it
silently produces sub-issues nobody can verify.

So, before decomposing, mentally draft an acceptance criterion for each piece
you intend to split out. Wherever you **cannot** write a concrete, testable
one — because the PRD is silent, vague, or self-contradictory there — that is
a gap in the document, not something to paper over.

For each gap, either:
- **Infer it** if the intent is genuinely unambiguous from context (state the
  assumption you are making), or
- **Ask the user** via the Clarify tool to supply the missing detail, or offer
  to draft the missing spec for them to confirm.

Do NOT proceed to create issues while any intended sub-issue would have a
vague or missing acceptance criterion. Resolve every gap first — refine the
PRD with the user until each piece is concretely verifiable. Only then move on.

A useful prompt to the user when the doc is too thin: list the specific
questions whose answers you need to write testable criteria (e.g. "what is the
expected response when the id is unknown — 404 or empty 200?"), rather than a
generic "please add more detail".

## Step 3 — Analyze the requirements

Extract from the PRD:

- **Core goal**: what the task is trying to achieve
- **Functional pieces**: distinct features or capabilities that can be built and shipped independently
- **Dependencies**: which pieces must be done before others (framework before plugins, data models before business logic, etc.)
- **Orthogonality**: pieces that can run in parallel without PR conflicts — keep them independent
- **Acceptance criteria**: concrete, verifiable conditions for each piece

## The label is the trigger — this skill NEVER applies it

This is the load-bearing rule of the whole skill. admiral's discoverer picks
up any sub-issue that carries the pickup label (the team's configured
`discoverer.require_label`, default `agent-ready`) in a pickable state. The
moment a sub-issue gets that label, it can be shipped on the next discoverer
tick.

**Decompose only creates the issue structure — it does NOT apply the pickup
label.** Applying the label kicks off autonomous shipping, so it is a separate,
explicit human action: the user reviews the decomposition first, then triggers
activation themselves (see Step 7). This skill must leave every sub-issue
UNlabeled.

Use `linear.save_issue` for everything (create when `id` is omitted, update
when `id` is passed). It supports `parentId`, `labels`, `blockedBy`,
`blocks`, and `state` directly — but do NOT pass `labels` with the pickup
label at any point in this skill.

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

## Step 5 — Create sub-issues (unlabeled)

For each functional piece from Step 3, create a sub-issue WITHOUT the pickup
label (this skill never adds it — see Step 7):

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

## Step 7 — Stop. Do NOT label. Hand off activation to the user

The issue structure is now complete (parent + sub-issues + acceptance
criteria + blocking relations), but every sub-issue is still UNlabeled, so
the discoverer will not touch any of it. This is intentional.

Do NOT apply the pickup label. Instead, tell the user the task is staged and
explain how to activate it when they are ready — activation is their explicit
trigger, made after they review the decomposition:

- **Manually**: add the pickup label (`discoverer.require_label`, default
  `agent-ready`) to the sub-issues in Linear. Add it to the foundation
  sub-issue first if they want to gate the rollout; blocked dependents can be
  labeled at the same time since their `blockedBy` relations already hold them
  back until the foundation completes.

If the user explicitly asks you (now or later) to activate the task, only then
apply the label via `linear.save_issue(id, labels: [...])` — and because all
blocking relations are already set (Step 6), labeling order is safe.

## Step 8 — Summary

Report to the user:

- Parent issue URL
- Sub-issue URLs with acceptance criteria summary
- Dependency chain (which sub-issue must complete before which)
- That the task is STAGED but NOT active (no pickup label applied), and how to activate it

Example summary:

```
Parent: [GEO-1] Build login feature
  └─ [GEO-2] Data models + auth interfaces (foundation)
     └─ [GEO-3] Email/password auth (core)
        └─ [GEO-4] OAuth integration (dependent)
     └─ [GEO-5] Session management (parallel with GEO-3)

Blocking relations: set (GEO-3/GEO-5 blocked by GEO-2; GEO-4 blocked by GEO-3)
Status: STAGED — no agent-ready label applied; nothing will ship yet.
To activate: add the "agent-ready" label to the sub-issues (or ask me to).
```

## Constraints

- This skill NEVER applies the pickup label — not to the parent, not to sub-issues. Creating the structure and activating it are separate actions; activation is the user's explicit trigger.
- Parent issue is the task definition, never a work unit — it is never labeled or picked up regardless.
- Sub-issue description = acceptance criteria, not implementation details — this is what admiral's verify loop judges against
- If the user provides vague requirements, ask clarifying questions before decomposing
- Do not create more than ~10 sub-issues — if a task is that large, suggest decomposing into multiple parent tasks
