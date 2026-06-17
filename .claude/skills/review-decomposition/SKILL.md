---
name: review-decomposition
description: Audit an existing Linear decomposition against admiral's two-level model and the discoverer/verify invariants — the read-only "lint" counterpart to decompose. Checks for executable slices stranded at the project root, L3+ nesting (which breaks admiral's one-hop verify cascade), labels that would mis-trigger or never trigger the discoverer (agent-ready on non-agent work, agent-task on parents, sub-issues in non-pickable states), canceled sub-issues that permanently stall task-verify, weak/missing acceptance criteria, and blocking-graph problems (cycles, dangling blockers). Produces a severity-rated report with concrete issue refs and remediation pointers; NEVER writes to Linear (fixes go through decompose / activate / manual edits). Takes a parent issue (audit its sub-tree), a project (audit all top-level issues + their sub-trees), or `top-level` (product-tier features). Invoke when the user says "/review-decomposition", "审一下拆分", "检查任务拆分", "review the decomposition", "audit this project's issues", "拆得对不对", or wants to validate an admiral issue tree before/after activation. Requires the Linear MCP.
---

# review-decomposition

Audit an existing Linear decomposition and report whether it conforms to
admiral's model and pickup/verify invariants. This is the **detect** counterpart
to `decompose`'s **prevent** guards — same relationship `code-review` has to
writing code: it is **read-only**, produces a **severity-rated report**, and
**never mutates Linear**. Remediation is the user's call, routed back through
`decompose` / `activate` / manual Linear edits.

## The model being audited (what "correct" means)

admiral expects a **two-level issue tree** (see the `decompose` skill for the
full statement):

```
Project            ← product (product-tier acceptance enumerates top-level issues)
 └─ Issue (L1)     ← task/feature: verification unit. Unlabelled, never shipped.
     └─ Sub (L2)   ← slice: execution unit. Labelled with the pickup label; one PR each.
```

The invariants this skill checks against — all verified in admiral's code:

- **Pickup** (`internal/discoverer/service.go`): the discoverer ships any issue
  matching `RequireLabel` + a `StateTypes` state + unassigned. It is
  **hierarchy-blind** — a labelled issue is picked regardless of where it sits.
- **Task-verify is one hop** (`internal/discoverer/state_advance.go`): a merged
  L2 walks up to its L1 parent and, when every sibling sub-issue is in a
  **completed** state, the parent task is verified. It does NOT recurse — so an
  L3 issue's completion never propagates, and a parentless issue never
  task-verifies.
- **Only `completed` counts as done** (`allSubsCompleted`): a sibling left
  `canceled` (PR closed unmerged) blocks the parent's task-verify **forever** —
  by design, a human must resolve it.
- **Blockers** (`BlockerWatcher`): an issue with an unresolved `blockedBy` is
  parked until every blocker reaches a **completed _or canceled_** state — both
  count as resolved. Note the asymmetry with task-verify: `canceled` *resolves*
  a blocker (the dependent unblocks) but does NOT count toward `allSubsCompleted`
  (a canceled sub stalls the parent's verify forever). Same state type, opposite
  effect in the two subsystems.

## Before you start

1. Confirm the Linear MCP is available (`list_issues` / `get_issue`). If not,
   stop and tell the user.
2. **Read the pickup config** so the audit uses the real gates, not guesses.
   Read `discoverer.require_label` and `discoverer.state_types` from admiral's
   config (`$XDG_CONFIG_HOME/admiral/config.yaml`, else
   `~/.config/admiral/config.yaml`).
   - `require_label`: the pickup label. If set, use it. If **empty**, pickup is
     judge-driven (no label gate) — note this, and fall back to the ecosystem
     convention `agent-ready` for label-related checks, flagging the assumption.
   - `state_types`: pickable state types. Default when unset:
     `["backlog", "unstarted"]`.
   - If the config file is unreadable, say so and proceed with defaults
     (`agent-ready` + `backlog`/`unstarted`), marking every config-dependent
     finding as "assumed defaults".

This skill is **read-only end to end** — it never calls `save_issue`,
`create_issue_label`, or any write. Say so up front if the user asks it to fix
things: it reports, it does not mutate.

## Step 1 — Resolve the target

| User intent | Examples | Resolution |
|---|---|---|
| **A parent issue** — audit its sub-tree | `/review-decomposition GEO-1`, a URL | `get_issue(GEO-1)`; audit it as an L1 task + its sub-issues. |
| **A project** — audit the whole tree | `/review-decomposition <project>`, "审这个 project" | enumerate the project's top-level issues + each one's sub-tree. |
| **Product tier** — top-level features only | `/review-decomposition top-level` | audit the project's top-level issues as features (lighter; skips per-slice criteria depth). |
| **Nothing given** | bare invocation | Infer from context (recent issue / project). Propose it back and confirm. Never guess silently. |

Resolve to a concrete project/team. If ambiguous, ask.

## Step 2 — Gather the tree (read-only)

Pull enough to evaluate every check below:

- For a **parent**: the issue itself (state, labels, `parent`, `description`)
  and its sub-issues (`list_issues(parentId: …)`), each with state type, labels,
  `description`, and — via `get_issue(…, includeRelations: true)` where needed —
  `blockedBy` relations.
- For a **project**: top-level issues (those whose `parent` is empty), then each
  one's sub-issues. An issue returned with a non-empty `parent` is an L2; track
  the depth so you can detect L3+.
- Record, per issue: identifier, title, `parent` (empty?), child count, labels,
  state + state **type**, whether `description` carries a concrete acceptance
  criterion, and blocking edges.

Prefer a few broad `list_issues` calls over many `get_issue` calls; only
`get_issue` individually where you need `description` or relations not in the
list payload.

## Step 3 — Run the checks

Severity is guidance for ranking; apply judgment. Each finding cites the
issue(s) and the invariant it violates.

### Structure / hierarchy
- **[HIGH] L3+ nesting** — a sub-issue that itself has sub-issues. Breaks the
  one-hop verify cascade: the grandchild's completion never reaches the L1.
  Remediation: flatten — move the grandchildren up to be siblings under the L1,
  or split the work differently.
- **[HIGH] Executable slice stranded at root** — a top-level (parentless) issue
  that carries the pickup label and/or `agent-task` and reads like a single-PR
  slice. It ships but never task-verifies (nothing walks up from it).
  Remediation: attach it under an L1 task parent.
- **[MEDIUM] Parent/feature carrying a work label** — an L1/top-level issue with
  `agent-task` or the pickup label. Parents are verification units, not work
  units. Remediation: remove the label from the parent.

### Labels / pickup
- **[CRITICAL] Pickup label on non-agent work** — the pickup label
  (`require_label`) on a human-only issue (no `agent-task`) or on a parent. The
  discoverer is hierarchy- and executor-blind: it will try to **ship** it.
  Remediation: remove the pickup label; only `agent-task` slices should carry it.
- **[MEDIUM] `agent-task` missing/misapplied** — an agent-doable slice without
  `agent-task` (won't be activatable via `/activate`), or `agent-task` on work
  that is clearly human-only.

### State (pickup + verify gates)
- **[MEDIUM] Sub-issue in a non-pickable state** — an `agent-task` slice whose
  state **type** is not in `state_types`. Even with the pickup label it will
  never be picked. Remediation: move it to a pickable state (e.g. Backlog/Todo).
- **[HIGH] Canceled sub-issue stalling task-verify** — a sub-issue in a
  `canceled` state under a parent whose other subs are progressing. `canceled`
  is NOT `completed`, so the parent's task-verify can never fire. Remediation:
  a human resolves the canceled sub (re-open + complete, or remove it from the
  task) to unblock verification.

### Acceptance criteria
- **[HIGH] agent-task slice with no/vague acceptance criterion** — the
  `description` lacks a concrete, black-box "done" condition. admiral's verify
  judge has nothing to judge against → the loop can't converge (it fails
  silently, not loudly). Remediation: enrich the description before activation;
  re-run `/decompose` on the parent if many slices are thin.
- **[LOW] human-only issue with no clear deliverable** — same gap, judged by a
  human; still worth a concrete done-condition.

### Dependency graph
- **[CRITICAL] Cycle in `blockedBy`** — A blocks B blocks A (directly or
  transitively). Every issue in the cycle is parked forever. Remediation: break
  the cycle.
- **[LOW] Dangling blocker edge** — a `blockedBy` pointing at a deleted issue.
  (A *canceled* blocker is NOT a problem: admiral treats `canceled` as resolved,
  so the dependent unblocks normally — do not flag canceled blockers.) A deleted
  blocker is skipped by admiral's resolver too, so this is advisory only:
  confirm the dependency was intentionally dropped, not lost.
- **[LOW] Missing/implied dependencies** — a foundation slice (shared types,
  framework, data model) that others clearly need but nothing blocks on; or
  parallel slices that look like they touch the same files (PR-conflict risk).
  Flag as advisory; you can't always tell from titles alone.

### Size / granularity
- **[MEDIUM] Oversized parent** — > ~10 sub-issues under one L1. Quality of
  acceptance criteria and review degrades; suggest splitting into multiple
  features.
- **[LOW] Granularity smell** — a slice that's clearly multi-PR (should split)
  or trivially small (should merge with a sibling).

## Step 4 — Report (read-only)

Produce a severity-grouped report. Lead with a one-line verdict and the counts,
then the findings, then a short remediation summary. Cite issue identifiers.

```
Decomposition audit: <parent/project>  (pickup label: <require_label or "judge-driven (assumed agent-ready)">; pickable states: <state_types>)

Verdict: <N> findings — <C> critical, <H> high, <M> medium, <L> low.

CRITICAL
  - [GEO-7] pickup label on human-only issue → discoverer will try to ship it. Remove the label.
HIGH
  - [GEO-12 → GEO-30] L3 nesting: GEO-30 is a sub-issue of sub-issue GEO-12 → never task-verifies. Flatten under GEO-1.
  - [GEO-9] agent-task slice, description has no verifiable acceptance criterion → verify can't converge. Enrich or re-decompose.
MEDIUM
  - [GEO-5] sub-issue in state "In Review" (type=started), not in pickable [backlog, unstarted] → won't be picked.
LOW
  - [GEO-2] foundation (shared types) but nothing blocks on it — confirm intended.

Remediation:
  - Re-run /decompose GEO-12 to flatten the L3 …
  - Remove pickup label from GEO-7 (manual) …
  - Nothing here is auto-fixed — this audit is read-only.
```

If the tree is clean, say so plainly (no manufactured findings) and confirm the
invariants checked.

## Constraints

- **Read-only.** Never call `save_issue` / `create_issue_label` / any write. If
  asked to fix, report + point at `decompose` / `activate` / manual edits.
- Use the **real** `require_label` / `state_types` from config; mark findings
  "assumed defaults" only when config is unreadable.
- Severity is guidance — apply judgment, and don't invent findings to fill
  categories. A clean tree gets a clean report.
- Title-only signals (granularity, file-overlap, implied deps) are advisory;
  say when a finding is a guess rather than a definite violation.
- This skill audits structure against admiral's invariants; it does NOT judge
  whether the PRD itself is the right product decision.
