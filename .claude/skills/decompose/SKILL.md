---
name: decompose
description: Decompose a task into a Linear issue hierarchy for the admiral autonomous loop — one parent issue holding the PRD, plus orthogonal sub-issues (foundation-first, dependency-ordered) each with acceptance criteria, wired with blocking relations. Classifies each sub-issue as agent-doable or human-only and tags the agent-doable ones with the `agent-task` label. Deliberately does NOT apply the pickup (`agent-ready`) label — nothing is shipped until the user explicitly activates the task. Invoke when the user says "/decompose", "decompose this", "拆任务", "拆分到 Linear", "break this down into issues", or wants to turn a PRD/design doc into admiral-ready Linear sub-issues. Requires the Linear MCP.
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
- **Executor**: for each piece, decide whether it is **agent-doable** or **human-only** (see Step 3.5)

## Step 3.5 — Classify each piece: agent-task vs human-only

For every functional piece you intend to split out, decide who can execute it.
This classification is orthogonal to the pickup trigger — it answers "is
admiral capable of doing this?", not "should admiral start now?".

**agent-task** — implementable by admiral autonomously:
- Writing or editing code in the repo
- Mechanical refactors / renames
- Adding tests, fixtures, config files
- Scripted migrations, dependency bumps
- Doc / comment updates that don't require new design decisions

**human-only** — requires a human:
- UX / visual design decisions, wireframes, mockups
- User research, interviews, stakeholder sync
- Vendor / legal / compliance coordination
- Infrastructure or architecture decisions that need human judgment
- Anything requiring access or accounts admiral doesn't have

A sub-issue that is "mostly code but needs one design decision first" should
usually be **split**: the design decision becomes its own human-only sub-issue
that blocks the implementation sub-issue. Mixing executors inside one issue
defeats the classification.

Acceptance criteria are required for **both** types — every sub-issue needs a
black-box "done" condition. The only difference is the judge: agent-task
issues are judged by admiral's verify loop; human-only issues are judged by a
human (PR review, deliverable handoff, Linear comment, etc.). Write the
criteria the same way regardless.

## The two label layers — what this skill does and doesn't apply

admiral's skill-layer pipeline uses **two orthogonal labels** on sub-issues:

1. **`agent-task`** (classification, applied by THIS skill) — marks a
   sub-issue as something admiral is capable of doing. Set in Step 5 based
   on the Step 3.5 classification. Stable metadata: stays on the issue for
   its lifetime.
2. **`agent-ready`** (pickup trigger, applied by `activate`, NOT here) —
   the team's configured `discoverer.require_label`. The moment a sub-issue
   gets this label, admiral's discoverer can ship it on the next tick.

**Decompose applies `agent-task` to agent-doable sub-issues; it NEVER
applies `agent-ready`.** Applying the pickup trigger kicks off autonomous
shipping, so it is a separate, explicit human action: the user reviews the
decomposition first, then runs `activate` (Step 7). Human-only sub-issues
carry neither label.

Use `linear.save_issue` for everything (create when `id` is omitted, update
when `id` is passed). It supports `parentId`, `labels`, `blockedBy`,
`blocks`, and `state` directly — pass `labels: ["agent-task"]` only on
agent-doable sub-issues, and NEVER pass the pickup label (`agent-ready`) at
any point in this skill.

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

## Step 4.5 — Ensure the `agent-task` label exists in the team

Before creating sub-issues, make sure the `agent-task` label exists in the
team you'll be writing to. `linear.save_issue(..., labels: ["agent-task"])`
will fail if the label has not been created — Linear's MCP does not
auto-create labels.

```
linear.list_issue_labels(team: <team-id>)
```

If `agent-task` is NOT in the returned list, create it:

```
linear.create_issue_label(team: <team-id>, name: "agent-task",
                          description: "Sub-issue is implementable by admiral (vs human-only).")
```

Do this once per team per session. If `agent-task` already exists, do nothing
and move on. Skip this step entirely if every functional piece from Step 3.5
was classified as human-only — no sub-issue will carry the label, so creation
is unnecessary (but still harmless).

## Step 5 — Create sub-issues (with `agent-task` for agent-doable pieces only)

For each functional piece from Step 3, create a sub-issue. Apply the
`agent-task` label IFF the piece was classified as agent-doable in Step 3.5.
Human-only sub-issues get NO labels here. NEVER apply the pickup label
(`agent-ready`) in this skill — that is `activate`'s job (Step 7).

```
linear.save_issue(
  team:        <same team>,
  project:     <same project>,
  parentId:    <parent identifier>,
  title:       <sub-task name>,
  description: <acceptance criteria — concrete, verifiable "done" condition for this sub-issue
                (judged by admiral verify for agent-task issues, by a human for human-only issues)>,
  labels:      <["agent-task"] for agent-doable; omit / [] for human-only>,
  state:       <a pickable state — see note>,
)
```

Record every sub-issue identifier (and its classification). Do this for ALL
sub-issues before moving on, so their IDs exist for the blocking step.

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
4. **Human-only sub-issues sit in the same graph** — block agent-task
   sub-issues on them whenever an upstream human decision is required (e.g.
   "design login screen" blocks "implement login screen"). admiral's
   BLOCKED gate is executor-agnostic: once a human closes the upstream
   issue in Linear, the downstream agent-task auto-unblocks on the next
   discoverer tick.

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

## Step 7 — Stop. Do NOT apply `agent-ready`. Hand off activation to the user

The issue structure is now complete (parent + sub-issues + acceptance
criteria + `agent-task` labels on agent-doable issues + blocking relations).
But no sub-issue carries the pickup trigger (`agent-ready`), so the discoverer
will not touch any of it yet. This is intentional.

Do NOT apply `agent-ready`. Instead, tell the user the task is staged and
explain how to activate it when they are ready — activation is their explicit
trigger, made after they review the decomposition:

- **`/activate <parent issue>`** — the dedicated activation skill. It
  confirms the project + parent issue with the user, then applies
  `agent-ready` only to sub-issues that carry `agent-task` (human-only ones
  are surfaced as "skipped"). Use this rather than labeling inline; it has
  the wrong-target safety gate.
- **Manually**: add `agent-ready` (the pickup label,
  `discoverer.require_label`) to specific sub-issues in Linear.

Either way, because all blocking relations are already set (Step 6), labeling
order is safe — dependents stay BLOCKED until their upstream sub-issues
(agent or human) reach a completed state.

Do not apply `agent-ready` yourself from within this skill, even if it seems
convenient — activation is a separate, explicitly-triggered step.

## Step 8 — Summary

Report to the user:

- Parent issue URL
- Sub-issue URLs with acceptance criteria summary, marked `[agent-task]` or `[human-only]`
- Dependency chain (which sub-issue must complete before which)
- Counts: how many `agent-task` vs how many `human-only`
- That the task is STAGED but NOT active (no `agent-ready` label applied), and how to activate it

Example summary:

```
Parent: [GEO-1] Build login feature
  ├─ [GEO-2] Data models + auth interfaces (foundation)        [agent-task]
  ├─ [GEO-3] Email/password auth (core)                        [agent-task]
  ├─ [GEO-4] OAuth integration                                 [agent-task]
  ├─ [GEO-5] Session management (parallel with GEO-3)          [agent-task]
  └─ [GEO-6] Visual design for login screen (precedes GEO-3)   [human-only]

Blocking relations: set
  GEO-3/GEO-5 blocked by GEO-2
  GEO-4 blocked by GEO-3
  GEO-3 blocked by GEO-6  (waits for human design before admiral can implement)

Classification: 4 agent-task, 1 human-only
Status: STAGED — no agent-ready label applied; nothing will ship yet.
To activate: /activate GEO-1   (will label the 4 agent-task issues with agent-ready; GEO-6 is for a human)
```

## Constraints

- This skill applies `agent-task` to agent-doable sub-issues and NEVER applies the pickup label (`agent-ready`) — not to the parent, not to sub-issues. Activation is the user's explicit trigger via `/activate`.
- Parent issue is the task definition, never a work unit — it is never labeled (no `agent-task`, no `agent-ready`) and never picked up.
- Sub-issue description = acceptance criteria, not implementation details — required for both agent-task and human-only issues (judged by admiral vs by a human respectively).
- If the user provides vague requirements, ask clarifying questions before decomposing.
- Do not create more than ~10 sub-issues — if a task is that large, suggest decomposing into multiple parent tasks.
