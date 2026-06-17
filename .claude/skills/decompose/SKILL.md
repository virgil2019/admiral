---
name: decompose
description: Decompose a task into Linear issues for the admiral autonomous loop. admiral expects a two-level issue tree — project (product) → L1 feature/task → L2 sub-issue/slice — and its verify loop only walks one hop, so two issue levels is the ceiling. Takes a parent reference — an existing Linear issue (sub-issues are created under it), the special sentinel `top-level` (with `project` accepted as a legacy alias; top-level issues are created directly under the project, no Linear parent), or a title for a new parent to create on confirmation. A recursive/full mode plans the whole feature→slice tree in one consolidated, reviewable pass (state-aware: builds features from a PRD when none exist, or batch-slices already-created features). Classifies sub-issues as agent-doable or human-only and tags agent-doable ones with `agent-task`. Top-level issues stay unlabeled — they are features, future-decomposable into ship-able slices. Deliberately does NOT apply the pickup (`agent-ready`) label — nothing is shipped until the user explicitly activates the task. Invoke when the user says "/decompose", "decompose this", "拆任务", "拆分到 Linear", "break this down into issues", "拆整个 project", "全部拆到 sub-issue", "--full" / "--recursive", or wants to turn a PRD / design doc / project description into admiral-ready Linear issues. Requires the Linear MCP.
---

# Decompose

Break a task into Linear issues ready for the admiral autonomous loop.

`decompose` slices content into pieces and attaches them in Linear. The
attachment point — an existing parent issue, a brand-new parent, or the
project root — is **input**, given by the user when invoking the skill.
`decompose`'s job is the slicing; creating parent issues is a Linear-native
operation that `decompose` only does as an explicit fallback (Step 4) when
the user asks for a parent that doesn't exist yet.

## The hierarchy admiral expects (read first — every mode obeys this)

One meta-principle governs where pieces attach. admiral's autonomous loop is
built around a **two-level issue tree**:

```
Project            ← the PRODUCT (product-tier acceptance enumerates its top-level issues)
 └─ Issue (L1)     ← a TASK / FEATURE: the verification unit. Never labelled, never
     │               shipped; admiral verifies it as a whole once all its slices complete.
     └─ Sub (L2)   ← a SLICE: the execution unit. Labelled `agent-task`; each ships as one PR.
```

- **project = product**, **L1 issue = task/feature** (the parent / verification
  unit), **L2 sub-issue = slice** (the execution unit). "feature" and "task" are
  the same object seen from two tiers — don't let the naming confuse you.
- **Two levels is the ceiling.** admiral's verify loop walks exactly ONE hop
  (a merged L2 → its L1 parent → checks siblings → verifies the L1). It does
  NOT recurse to grandparents. So an L3 issue's completion never propagates up,
  and the verify cascade silently breaks. Never decompose an issue that is
  itself already a sub-issue (the Step 1 guard enforces this).
- **Executable work never lives at the root.** Only L2 slices carry labels and
  get picked up; an L1/top-level issue with executable content stranded at the
  root will never trigger task-verify (nothing walks up from it) and, per
  design, carries no label so the discoverer won't ship it either. If a piece
  is concrete enough to implement-and-ship, it belongs under a parent as an L2
  slice, not at the root as a "feature".

Everything below is the mechanics of producing exactly this shape.

## Before you start

Confirm you have Linear MCP available. If `linear.save_issue` is not
available, tell the user and stop.

## Step 1 — Resolve the parent reference

The user's invocation determines the attachment point. Resolve it BEFORE any
Linear writes. The valid shapes:

| User intent | Examples | Resolution → mode |
|---|---|---|
| **Existing issue** — slice under it | `/decompose GEO-5`, URL, "decompose this issue" with a clear ref | `linear.get_issue(GEO-5)` to verify exists → **sub-issue mode**. PRD source = the issue's `description`. |
| **Project root** — produce top-level features | `/decompose top-level`, `/decompose --top-level`, "拆 project / 顶层", "为 project 创建 issues"; `/decompose project` / `--project` / `--project-root` accepted as legacy aliases | No Linear parent → **top-level mode**. PRD source = conversation content provided by the user. |
| **Title for a new parent** — issue doesn't exist yet | `/decompose "Build login feature"`, "拆: build login" with no matching ID | `linear.list_issues(query: "<title>")` to search. Zero matches → **create-then-sub-issue mode** (Step 4 will confirm + create). Multiple matches → ask user to pick. Exactly one match → treat as existing. |
| **Whole tree at once** — features + their slices in one pass | `/decompose --full`, `/decompose --recursive`, "拆整个 project", "全部拆到 sub-issue", "把 project 一次拆到底" | **recursive mode** → see "Recursive (full) mode" section below. State-aware: builds L1 features from a PRD when none exist, then slices every feature; or, when features already exist, only slices the ones still missing sub-issues. |
| **Nothing given** | bare `/decompose` | Infer from conversation context. If a recent Linear issue / project / title is clearly the intent, propose it back to the user and wait for confirmation. If not clear, ask explicitly. **Never guess silently.** |

Identifier heuristics (the LLM applies these to disambiguate the user's input):
- Looks like `[A-Z]+-\d+` or a `linear.app/...` URL → treat as an issue
  reference.
- Literal `top-level` / `--top-level` / "顶层" → top-level mode. Legacy aliases also accepted: `project` / `--project` / `--project-root` (the canonical name avoids overloading Linear's own `project` field, but accept either to be ergonomic).
- `--full` / `--recursive` / "一次拆到底" / "全部拆到 sub-issue" → recursive mode.
- Anything else, especially quoted strings or phrases → title text; search.

Rules:
- **Always confirm before any Linear write.** Step 1 is read-only
  (`get_issue`, `list_issues`). Writes happen only in Steps 4 and 6, and
  Step 4 always asks first.
- If the user gave a top-level sentinel (`top-level`, `project`, etc.),
  also resolve which project — usually the one tied to the current repo
  per admiral's config, but ask if ambiguous.
- Record the resolved **attachment mode** (one of: `sub-issue-mode`,
  `top-level-mode`, `create-then-sub-issue-mode`, `recursive-mode`) and the
  chosen `team` / `project` ids. Every later step branches on the mode.
- **Level-3 guard (sub-issue mode).** Before slicing under an existing issue,
  check whether that issue is **itself already a sub-issue** — `get_issue`
  exposes the issue's parent reference; if it has a parent, the issue is an L2 slice.
  Decomposing it would create an L3 issue, which **breaks the verify cascade**
  (a merged L3 walks up only to its L2 parent — the L2's completion never
  propagates to the L1, so the task never auto-verifies). STOP and tell the
  user. Offer the right alternatives: attach the new slices under the issue's
  **L1 parent** instead (so they become siblings, still L2), or reconsider
  whether this work is really a separate slice. Do NOT silently create L3.
- **Granularity sanity (top-level mode).** Top-level issues must be *features*
  (coarse, themselves further-sliceable), not *slices* (concrete, directly
  implement-and-ship work). If the pieces you're about to create read like
  slices — a single PR's worth of code with a crisp acceptance criterion —
  warn the user: "these look like executable slices, not features; attaching
  them at the project root means they're never picked up (no label) and never
  task-verified (nothing above them). Did you mean to slice them under a parent
  (sub-issue mode), or run recursive mode?" Let the user redirect before any write.
- **PRD content pre-check** (top-level mode, create-then-sub-issue mode, and
  recursive mode in its "no features yet" state): in these the PRD source is
  conversation content, not an existing Linear issue. If the user has not
  provided PRD content in this conversation yet, ask for it now BEFORE moving
  to Step 2. Step 2's detail gate is the wrong place to discover the user never
  typed a PRD — that produces a confusing back-and-forth. (Sub-issue mode, and
  recursive mode when features already exist, skip this check: the PRD comes
  from the existing issue's / each feature's `description`.)

## Recursive (full) mode — orchestration

This mode exists to kill the per-feature-trigger tedium: instead of running
`/decompose <feature>` once per feature, plan the **whole feature→slice tree**
in one go, present it as **one consolidated, reviewable plan**, and materialize
on a single approval. It does not invent a new way to slice — it loops the
same Steps 2–7 over each feature. Two non-negotiables carry over: the Step 2
detail gate (every slice needs a verifiable acceptance criterion) and Step 8
(it still NEVER applies `agent-ready`).

**Separate planning from materialization.** Planning the tree is read-only and
fully derivable from the doc; the human gate is reviewing that plan, not
triggering each feature. So: plan everything first (no Linear writes), show the
user, then write on approval.

### State-aware: only fill the gap (idempotent)

Inspect the project's existing top-level (L1) issues first, then act by state:

| Project state | What recursive does | Per-feature PRD source |
|---|---|---|
| **No L1 features yet** (only a PRD in conversation) | Plan L1 features **and** their L2 slices; materialize both. | conversation PRD |
| **L1 features exist, no slices** | Do NOT recreate features. Slice each feature into L2 sub-issues. | each feature's own `description` |
| **Partially sliced** | Only slice features that still have **zero** sub-issues; leave existing slices untouched. | each feature's own `description` |

Re-running is safe: it only adds slices to features that lack them — never
duplicates a feature or an existing slice. When features already exist, their
`description` is the mini-PRD to slice from (same contract as sub-issue mode);
the conversation PRD is used only to *create* features in the first state.

### Flow

This mode runs its own sequence (labelled **R1–R7** to avoid colliding with the
global Step numbers); where it reuses a global Step it says so explicitly.

- **R1 — Resolve + detect state.** Resolve the project (Step 1 rules). Enumerate
  the project's **top-level issues**, counting only issues whose `parent` is
  empty — a returned issue with a non-empty `parent` is an L2 slice, exclude it
  (same deterministic test as the Step 1 level-3 guard). From these, determine
  the state in the table above. Throughout R1–R7, **"unsliced top-level issue"
  means a top-level issue with zero sub-issues** (matching the state table);
  features that already have any sub-issue are treated as sliced and skipped.
- **R2 — Granularity gate (before slicing anything).** A top-level issue
  existing does NOT make it a feature to slice. Classify each *unsliced*
  top-level issue:
  - **Coarse feature** — broad goal, would naturally break into several
    independently-shippable slices, no single crisp PR-sized "done" → plan L2
    slices for it (R3).
  - **Already a slice** — single PR's worth of work with a concrete, verifiable
    acceptance criterion (often already started/done, or grouped under a Linear
    **milestone** alongside peers). Do NOT slice it — slicing would create an L3
    issue and break the verify cascade. Leave it and surface it as "already
    slice-grained — not sliced".
  If MOST top-level issues are slice-grained, the project isn't organized as
  feature→slice at all (milestone-grouped flat slices is a different, valid
  shape — admiral just won't task-verify them, since nothing walks up from a
  parentless issue). Say so and stop; do not invent a feature layer or slice
  slices. This is the recursive-mode counterpart of Step 1's granularity sanity.
- **R3 — Plan only (no writes):**
  - No-features state: plan L1 features from the PRD, then for each feature plan
    its L2 slices (acceptance criteria + agent-task/human-only classification +
    intra-feature blocking) per global Steps 2/3/3.5.
  - Existing-features states: for each top-level issue classified **coarse
    feature** in R2, plan its L2 slices from the issue's `description`. Skip the
    ones classified **already a slice**.
- **R4 — thin-PRD flag.** If a feature's PRD/description is too thin to write
  concrete, verifiable slice criteria (the global Step 2 gate fails for it), do
  NOT emit guess-work slices. Flag that feature and either ask the user to
  enrich it or mark it "feature-only — slice later", and exclude it from this
  pass.
- **R5 — Size guard.** If the full plan is large (rough thresholds: > ~8
  features, or > ~30 total issues), warn the user and offer to materialize in
  feature-batches rather than all at once — still batched (no per-feature manual
  command), just split into a few reviewable chunks so neither the
  acceptance-criteria quality nor the human review degrades. The "~10 issues per
  pass" cap (global Step 6 / Constraints) applies here *per feature's slices*.
- **R6 — One consolidated review.** Present the entire planned tree — features →
  slices, labels, blocking edges, and any flags (thin-PRD deferrals, size
  batching, and R2 "already slice-grained — not sliced" items) — and wait for a
  single explicit approval. This is the real gate (after approval, `activate`
  can ship it), so it must not be a rubber stamp.
- **R7 — Materialize on approval, then stop.** Run global Steps 4–7 across the
  plan: create the L1 features that don't exist yet (Step 4-style, unlabelled),
  then the L2 slices under every in-scope feature (Step 6 sub-issue form,
  `agent-task` on agent-doable slices), then wire blocking (Step 7). Then **stop
  at global Step 8** — do not apply `agent-ready`. Report the full tree (Step 9),
  noting any features deferred via thin-PRD flag (R4) or size batching (R5), and
  any top-level issues left untouched as already slice-grained (R2).

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
- **Ask the user** to supply the missing detail, or offer to draft the
  missing spec for them to confirm.

Do NOT proceed to create issues while any intended piece would have a vague
or missing acceptance criterion. Resolve every gap first — refine the PRD
with the user until each piece is concretely verifiable. Only then move on.

**Top-level mode**: the same principle applies at a coarser grain. A
top-level "feature" is decomposable when you can state its goal + scope
boundaries cleanly enough that a future `/decompose <top-level>` pass has a
verifiable mini-PRD to work from. Top-level issues are themselves mini-PRDs
for the next slicing pass.

## Step 3 — Analyze the requirements

Extract from the PRD:

- **Core goal**: what the task is trying to achieve
- **Functional pieces**: distinct features or capabilities that can be built and shipped independently
- **Dependencies**: which pieces must be done before others (framework before plugins, data models before business logic, etc.)
- **Orthogonality**: pieces that can run in parallel without PR conflicts — keep them independent
- **Acceptance criteria**: concrete, verifiable conditions for each piece
- **Executor**: when planning slices (sub-issue / create-then-sub-issue / recursive mode), classify each piece as agent-doable or human-only (see Step 3.5); skip for the feature-planning level of top-level / recursive mode

## Step 3.5 — Classify each piece: agent-task vs human-only

**Top-level mode**: skip this step. Top-level issues are features — too coarse
to classify as agent-task; classification happens at the next slicing pass
when the user runs `/decompose <top-level>`.

For any slice-producing mode (sub-issue / create-then-sub-issue / recursive),
decide for every piece who can execute it. This classification is orthogonal
to the pickup trigger — it answers "is admiral capable of doing this?", not
"should admiral start now?".

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
issues are judged by admiral's verify loop; human-only issues are judged by
a human (PR review, deliverable handoff, Linear comment, etc.). Write the
criteria the same way regardless.

## The two label layers — what this skill does and doesn't apply

admiral's skill-layer pipeline uses **two orthogonal labels** on sub-issues:

1. **`agent-task`** (classification, applied by THIS skill in any
   slice-producing mode: sub-issue / create-then-sub-issue / recursive) —
   marks a sub-issue as something admiral is capable of doing. Set in Step 6
   based on the Step 3.5 classification. Stable metadata: stays on the issue
   for its lifetime.
2. **`agent-ready`** (pickup trigger, applied by `activate`, NOT here) —
   the team's configured `discoverer.require_label`. The moment a sub-issue
   gets this label, admiral's discoverer can ship it on the next tick.

**Decompose applies `agent-task` to agent-doable sub-issues; it NEVER
applies `agent-ready`.** Applying the pickup trigger kicks off autonomous
shipping, so it is a separate, explicit human action: the user reviews the
decomposition first, then runs `activate` (Step 8). Human-only sub-issues
carry neither label. **Top-level issues (`project` mode) carry neither
label** — they are features, not work units.

Use `linear.save_issue` for every write (create when `id` is omitted, update
when `id` is passed). It supports `parentId`, `labels`, `blockedBy`,
`blocks`, and `state` directly — pass `labels: ["agent-task"]` only on
agent-doable sub-issues in a slice-producing mode, and NEVER pass the pickup
label (`agent-ready`) at any point in this skill.

## Step 4 — Materialize parent (create-then-sub-issue mode only)

**Other modes** (sub-issue mode with existing parent, top-level mode): skip
this step.

If Step 1 resolved to **create-then-sub-issue mode** (the user gave a title
that doesn't match any existing issue), confirm explicitly:

> No existing issue matches `<title>`. Create a new parent with this title
> + your PRD content as the description, then slice it under that parent?
> [y/n]

On **yes**:

```
linear.save_issue(
  team:        <chosen team>,
  project:     <chosen project>,
  title:       <title from user>,
  description: <full PRD content — the ground-truth requirement admiral's verify loop judges against>,
)
```

Record the returned identifier — this is the parent for the rest of the flow.
Do NOT label the parent. The parent is the task definition, never a work
unit — the discoverer only ever picks up its sub-issues.

On **no**: stop, and tell the user to pick a different attachment (existing
issue, `project` sentinel, or create the parent themselves first).

## Step 5 — Ensure the `agent-task` label exists in the team

**Skip this step entirely if**:
- Top-level mode (no `agent-task` will be applied), OR
- Every piece from Step 3.5 was classified as human-only

Otherwise, make sure the `agent-task` label exists in the team you'll be
writing to. `linear.save_issue(..., labels: ["agent-task"])` will fail if
the label has not been created — Linear's MCP does not auto-create labels.

```
linear.list_issue_labels(team: <team-id>)
```

If `agent-task` is NOT in the returned list, create it:

```
linear.create_issue_label(team: <team-id>, name: "agent-task",
                          description: "Sub-issue is implementable by admiral (vs human-only).")
```

Do this once per team per session. If `agent-task` already exists, do
nothing and move on.

## Step 6 — Create the issues

For each piece from Step 3, create an issue at the right attachment point.
The call differs by mode:

**Sub-issue mode** (existing parent or just-materialized parent in Step 4):

```
linear.save_issue(
  team:        <same team>,
  project:     <same project>,
  parentId:    <parent identifier>,
  title:       <sub-task name>,
  description: <acceptance criteria — concrete, verifiable "done" condition
                (judged by admiral verify for agent-task issues, by a human for human-only issues)>,
  labels:      <["agent-task"] for agent-doable; omit / [] for human-only>,
  state:       <a pickable state — see note>,
)
```

**Top-level mode** (project root, no parent):

```
linear.save_issue(
  team:        <same team>,
  project:     <project identifier>,
  // NO parentId — issue attaches directly to the project
  title:       <feature name>,
  description: <mini-PRD for this feature — enough that a future /decompose <this-issue> has a verifiable doc to slice>,
  // NO labels — top-level issues carry neither agent-task nor agent-ready
  state:       <a pickable state — see note>,
)
```

Record every issue identifier (and, in sub-issue mode, its classification).
Do this for ALL issues before moving on, so their IDs exist for the blocking
step.

**Decompose so that:**

1. **Foundation is its own issue, depended on by the rest** — frameworks,
   data models, global variables, shared interfaces, base types. Ships
   first; everything else blocks on it.
2. **Parallel pieces are orthogonal** — no shared mutable state, no
   overlapping file edits, so their PRs never conflict. If two pieces would
   touch the same file, either merge them into one issue or split along an
   interface so each owns its own files.
3. **At most ~10 issues per pass.** If you need more, suggest the user
   split into multiple decomposition passes (e.g. multiple top-level
   features, each later sliced separately). This is the single source of
   truth for the cap; recursive mode (R5) applies it *per feature's slices*
   and batches a large tree rather than overriding it.
4. **Human-only sub-issues sit in the same graph** (any slice-producing mode) —
   block agent-task sub-issues on them when an upstream human decision is
   required (e.g. "design login screen" blocks "implement login screen").
   admiral's BLOCKED gate is executor-agnostic: once a human closes the
   upstream issue in Linear, the downstream agent-task auto-unblocks on the
   next discoverer tick.

**Note on state:** the issue must land in a state whose type is in the
discoverer's `state_types` (default `backlog` / `unstarted`) or it won't be
pickable even with the pickup label. New issues usually default to a backlog
state; set `state` explicitly (e.g. "Backlog" or "Todo") if unsure.

## Step 7 — Set blocking relations

Now that every issue ID exists, wire dependencies. For each dependent issue
B that needs prerequisite A done first:

```
linear.save_issue(id: <B identifier>, blockedBy: [<A identifier>, ...])
```

`blockedBy` is append-only and accepts multiple blockers. Set every
dependency edge here. admiral's BlockerWatcher reads these `blocked_by`
relations: an issue with an unresolved blocker is parked BLOCKED and
auto-resumes once its blockers reach a completed state — so foundation ships
first, dependents wait, naturally.

All modes use the same mechanism: top-level features can block each other
(e.g. "Auth layer" blocks "Comments"); sub-issues can block each other or be
blocked by human-only sub-issues. In recursive mode, set both layers — feature
↔ feature edges and, within each feature, slice ↔ slice edges.

## Step 8 — Stop. Do NOT apply `agent-ready`. Hand off to the user

The issue structure is now complete. No issue carries the pickup trigger
(`agent-ready`), so the discoverer will not touch any of it yet. This is
intentional.

**Sub-issue modes** — activation is the next step:

> Task is staged. To kick off admiral:
> - **`/activate <parent>`** — applies `agent-ready` only to sub-issues
>   carrying `agent-task` (human-only ones are surfaced as skipped).
>   Recommended path; has wrong-target safety gate.
> - **Manually**: add `agent-ready` to specific sub-issues in Linear.
>   **You are responsible for skipping human-only sub-issues** — admiral's
>   discoverer cannot tell `agent-task` from human-only and will try to
>   ship anything carrying `agent-ready`. `/activate` enforces this check;
>   the manual path does not.

**Top-level mode** — there is no direct activation; the next step is
per-feature slicing:

> Top-level features created. Nothing is queued for admiral yet — features
> are too coarse for the discoverer to ship directly. Next step: pick a
> top-level issue and run **`/decompose <that-issue>`** to slice it into
> agent-task sub-issues. After slicing, run `/activate <that-issue>` to
> kick admiral off for that feature.

**Recursive mode** — features now have their slices, so activation is
per-feature (each feature is an independently-activatable task):

> Full tree staged: <N> features, <M> agent-task slices. Nothing carries
> `agent-ready` yet. Activate per feature with **`/activate <feature>`** —
> run it for each feature you want admiral to start on (they activate
> independently; you don't have to start them all at once). Any features
> deferred (thin PRD) or held back by the size guard are listed above and
> still need slicing before they can be activated.

Either way, because all blocking relations are already set (Step 7),
labeling order is safe — dependents stay BLOCKED until their upstream issues
(agent or human) reach a completed state.

Do not apply `agent-ready` yourself from within this skill, even if it
seems convenient — activation is a separate, explicitly-triggered step.

## Step 9 — Summary

Report to the user. Layout depends on the attachment mode.

**Sub-issue mode example:**

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
To activate: /activate GEO-1
```

**Top-level mode example:**

```
Project: snipd (proj-abc123)
Top-level features (no parent, attached directly to project):
  ├─ [GEO-10] HTTP API foundation
  ├─ [GEO-11] TTL sweeper                  (blocked by GEO-10)
  ├─ [GEO-12] Auth layer                   (blocked by GEO-10)
  └─ [GEO-13] Admin UI                     (blocked by GEO-12)

Status: STAGED — top-level features only; no agent-task / agent-ready labels yet.
Next step: pick a feature and run `/decompose <feature-id>` to slice it
into agent-task sub-issues. e.g. `/decompose GEO-10`.
```

**Recursive mode example** (full tree — features + their slices):

```
Project: snipd (proj-abc123)
  ├─ [GEO-10] HTTP API foundation
  │    ├─ [GEO-14] Router + middleware (foundation)            [agent-task]
  │    └─ [GEO-15] Health/readiness endpoints                  [agent-task]
  ├─ [GEO-11] TTL sweeper            (feature blocked by GEO-10)
  │    └─ [GEO-16] Sweep loop + config                         [agent-task]
  └─ [GEO-12] Auth layer             (feature blocked by GEO-10)
       ├─ [GEO-17] Token model + interfaces (foundation)       [agent-task]
       └─ [GEO-18] Login screen design (precedes GEO-19…)       [human-only]

Deferred (thin PRD, not sliced): [GEO-13] Admin UI — needs spec before slicing.
Classification: 5 agent-task, 1 human-only across 3 features.
Status: STAGED — no agent-ready label applied; nothing will ship yet.
To activate per feature: /activate GEO-10  (then GEO-11, GEO-12 …)
```

## Constraints

- This skill applies `agent-task` to agent-doable slices (any slice-producing mode: sub-issue / create-then-sub-issue / recursive) and NEVER applies the pickup label (`agent-ready`) — not to parents, not to sub-issues, not to top-level features. Activation is the user's explicit trigger via `/activate`.
- Parents / top-level issues / features are never labeled (no `agent-task`, no `agent-ready`) and never picked up by the discoverer.
- Implicit parent/feature creation happens only in the create-then-sub-issue branch and in recursive mode's "no features yet" state — both after explicit user confirmation (Step 4 / the recursive consolidated review). Otherwise this skill WRITES sub-issues / top-level issues only — it never creates parents implicitly.
- **Two issue levels is the ceiling** (project → L1 feature/task → L2 slice). Never decompose an issue that is itself a sub-issue (Step 1 level-3 guard); never strand executable work at the project root. admiral's verify loop walks one hop only, so deeper nesting silently breaks task-verify.
- Sub-issue description = acceptance criteria, not implementation details — required for both agent-task and human-only issues (judged by admiral vs by a human respectively). Top-level description = mini-PRD for the feature (must support future slicing).
- If the user provides vague requirements, ask clarifying questions before decomposing.
- Do not create more than ~10 issues in one pass — if a task is that large, suggest splitting into multiple decomposition passes (or, in top-level mode, multiple project decompositions). Recursive mode plans many features at once but still respects this per-feature (≤ ~10 slices each) and uses its size guard to batch a large tree into reviewable chunks.
