# Contributing to admiral

## PR checklist convention

Every PR opened against this repo MUST contain a `## Test plan` section
with two sub-sections:

````markdown
## Test plan

### Agent-completed (this branch — already verified)

- [x] item the author has already verified
- [x] another item

### Human-required (please run before merge)

- [ ] step that requires a human to perform
- [ ] another human step
````

### Why this convention exists

The `pr-checklist` GitHub Actions workflow (`.github/workflows/pr-checklist.yml`)
gates PR merge readiness on two signals:

1. **No unchecked task boxes** (`- [ ]`) remain in the PR body.
2. **The `human-verified` label** is present on the PR.

The label is the critical part: it can only be applied by a repo collaborator
who is not the PR author. An agent can pre-fill every checkbox but cannot
self-apply the label — this closes the self-ticking bypass.

### When merging

- The PR author completes the `Agent-completed` section (all boxes `[x]`).
- A human collaborator reviews the `Human-required` section, performs each
  listed step, and applies the `human-verified` label in the GitHub UI once
  all items are done.
- The workflow check turns green only when both conditions are met.

### Workflow test coverage

The workflow parses sample PR bodies covering: all-checked / mixed /
no human-required section / malformed inputs. Tests live alongside the
workflow file and can be run locally with `act` or in a PR fork.
