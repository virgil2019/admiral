# Contributing to admiral

## PR checklist convention

Every PR opened against this repo should contain a `## Test plan` section
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

It makes the steps a human must run before merge easy to see.
`Agent-completed` lists what the author already verified; `Human-required`
lists what still needs a person (end-to-end paths automated tests can't
cover). Keep `Human-required` to steps that really need a human; write
`(none)` when there are none. This is a writing convention, not a CI gate.

## Branch protection

The `main` branch is protected. All merges require:

- **Status checks**: the `test` workflow must pass (runs `go test` on every push/PR).
- **Reviews**: at least 1 human reviewer (no agent reviewers). An agent may
  prepare a PR but it sits until a human collaborator approves it.
- **No force-push**: force-pushing to `main` is blocked for everyone.
- **No branch deletion**: deleting the `main` branch is blocked.

These rules are enforced by GitHub branch protection settings
(Settings → Branches → Add rule for `main`).
