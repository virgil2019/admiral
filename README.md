# admiral

A Go daemon that bridges a single Telegram chat to a single team-cli team
— either `oh-my-codex` (`omx`) or `oh-my-claudecode` (`omc`) — via
`<bin> team api`. Mac-local MVP. Long-polling on TG, whitelist by TG
user id, 1 bot : 1 team.

Repo location: `/Users/georgehuang/Program/george/admiral` (repo root —
`go run ./cmd/admiral` from here).

## Prerequisites

- Go 1.22+
- One of:
  - `omx` CLI in PATH (or absolute path via `session.cli_bin_path`), or
  - `omc` CLI likewise.
- `/usr/bin/script` (only when `launch.mode: pty` — ships on macOS)
- A Telegram bot token (BotFather → `/newbot`)

## Setup

1. Copy the example config:
   ```
   mkdir -p ~/.config/admiral
   cp config.example.yaml ~/.config/admiral/config.yaml
   ```
2. Get a bot token from [@BotFather](https://t.me/BotFather) and paste it into
   `bot_token`.
3. Get your TG user id (message [@userinfobot](https://t.me/userinfobot)) and
   put it into `allowed_tg_user_ids`. Also fill `session.tg_chat_id` — for a
   1:1 DM chat, this is the same as your user id.
4. Pick the provider and fill the rest:
   - `session.provider` — `omx` or `omc`. Defaults to `omx` if omitted.
   - `session.cli_bin_path` — absolute path to the matching team-cli binary.
     `session.omx_bin_path` is accepted as a deprecated alias.
   - `session.cwd` — repo or working dir the team operates in.
   - `launch.worker_count` — N worker panes (e.g. 3).
   - `launch.agent_type` — `codex` | `claude` | `gemini`.
   - `launch.bootstrap_task` — free-form task string. The team name is
     **derived** from this via the team-cli's sanitize rule (lowercase,
     non-alnum collapsed to `-`, trimmed, max 30 chars).
   - `session.team_name` — MUST equal `sanitize(launch.bootstrap_task)` or
     the bridge refuses to boot. Example: `bootstrap_task: "vibe bridge
     session"` ⇒ `team_name: "vibe-bridge-session"`.

## Run

```
go run ./cmd/admiral
```

Or with a non-default config path:

```
go run ./cmd/admiral --config /path/to/config.yaml
```

## TG commands

| Command | Effect |
|---|---|
| `/start` | Launch the team (idempotent) |
| `/status` | Team summary |
| `/stop` | Request team shutdown (via leader mailbox) |
| `/help` | List commands |
| `/whoami` | Print identity |
| plain text | Forwarded to team leader as a mailbox message |

## Team launch semantics

`/start` invokes the team-cli's spawn verb:
```
<cli_bin_path> team <worker_count>:<agent_type> "<bootstrap_task>"
```
The team identifier is derived by the team-cli from the task string
(sanitize rule: lowercase, collapse non-alnum runs to `-`, trim, max 30
chars), and the bridge enforces that `session.team_name` equals that
sanitized form. After launch the bridge polls `team api get-summary` for
up to 60s to confirm the team is up before replying "Team <name> is up."
in TG.

## Launch modes (config: `launch.mode`)

- **`pty`** (default for `omx` on macOS). The bridge prepends
  `launch.pty_command` (default `/usr/bin/script -q /dev/null`) to the
  team-cli argv so the binary sees a real terminal. Logs a
  `WARN launch.mode=pty using_script_wrapper` line on every `/start`.
  Combined stdout+stderr is captured and surfaced in launch-failure
  messages (under `pty`, `script(1)` redirects child stderr into the pty,
  so a stderr-only pipe is always empty).
- **`direct`** (default for `omc` everywhere; default for `omx` on
  non-darwin). No wrapper. If the team-cli exits with the
  "stdin is not a terminal" error, the bridge replies with instructions
  to either flip `launch.mode: pty` or pre-launch from a terminal, and
  keeps the daemon running so `/status` still works.

`launch.pty_command` is user-configurable — override if you need a
different PTY wrapper (Linux `script` has different flag semantics).

## Provider matrix

| Capability | omx | omc |
|---|---|---|
| `team api await-event` (long-poll event stream) | ✅ | ❌ (mailbox polling fallback) |
| `team api read-idle-state` | ✅ | ❌ |
| `team api read-stall-state` | ✅ | ❌ |
| `team api get-summary` / `send-message` / `mailbox-*` / `*-shutdown-*` | ✅ | ✅ |
| Worker leader naming | virtual `leader-fixed` | concrete `worker-1` (configurable) |
| Auto-submit dispatched message into worker pane | hooks (built-in) | bridge sends explicit `tmux send-keys Enter` post-API |

Under `omc`, the bridge runs a periodic poll (interval =
`event_stream.idle_backoff_ms`) that drains undelivered tg-bridge
mailbox messages and confirms team liveness via `get-summary`. The
synthesized event types omx emits — `task_completed`, `task_failed`,
`approval_decision`, `all_workers_idle`, `shutdown_ack` — are NOT
pushed to TG under omc; only worker→tg-bridge replies are forwarded.
`/status` under omc omits the `stalled:` line.

### omc-specific protocol notes

- **Launch goes through a real PTY.** `omc team` is foreground for ~1s
  while it creates a tmux session and `tmux send-keys`-launches `claude`
  into worker panes. With stdin = /dev/null it EOFs before the
  send-keys finishes. Admiral allocates a real PTY via `github.com/creack/pty`
  for omc launches; `launch.mode` and `launch.pty_command` are ignored.
- **First-run trust dialog.** When `claude` starts in a fresh `cwd` it
  blocks on a "trust this folder?" prompt. Admiral cannot resolve this
  remotely. On the very first `/start` after a new `cwd`,
  `tmux attach -t omc-team-<team>-<id>` on the Mac, press `1`-Enter in
  each worker pane, then detach (`C-b d`). Subsequent launches in that
  `cwd` go fully unattended (claude persists the trust grant).
- **Plain text gets wrapped with reply protocol.** omc's dispatch
  layer doesn't auto-instruct claude to reply via the send-message API,
  so the bridge prepends an explicit "to reply, run `omc team api
  send-message --input '{...}'`" stub to each forwarded TG plain text.
  Without this, claude tends to print short answers in-pane and the
  user never sees them. omx provider does not wrap.
- **Auto-submit Enter.** After every `send-message` to a worker, the
  omc client looks up the worker's `pane_id` from the team manifest and
  fires `tmux send-keys -t <pane> Enter` to actually submit the
  dispatched text into claude. Best-effort; failures fall through.

### Session config (`session.leader_worker`)

The TG plain-text routing target. Defaults are provider-specific:
- `omx`: `leader-fixed` (omx's virtual dispatcher worker)
- `omc`: `worker-1` (omc's first concrete worker — there is no agent
  in omc's leader pane; sending to `leader-fixed` would land in a
  dead mailbox)

Override only if you want plain-text routed to a specific worker (e.g.
`worker-3`).

## Event loop lifecycle

The push goroutine starts when any of: `/start` succeeds; a plain-text
message is sent successfully; or at boot if a cursor is persisted in
SQLite AND `get-summary` confirms the team is still up. (The cursor
condition is a no-op under omc since polling does not write a cursor.)
Boot with a non-empty cursor but a missing team does **not** start the
loop — the bridge waits for `/start`.

## Inbound delivery guarantee

At-least-once. Dedupe key is the Telegram `update_id`. Every inbound
update is persisted to `tg_updates (update_id PRIMARY KEY)` with
`INSERT OR IGNORE` before processing, and marked processed in the same
transaction as the outbound `messages` row. A bridge crash between
persist and `send-message` is recovered on next boot — pending rows are
drained before the long-poll resumes.

## Mac sleep / wake > 23h

On every successful long-poll, the bridge persists
`last_successful_poll_at`. On boot (or after any gap), if the gap
exceeds 23h (safety margin under TG's 24h retention window), a one-time
`Bridge woke after <duration>; some older updates may have been dropped
by Telegram.` is pushed. Re-push is suppressed for 1h to avoid spam on
crash loops.

## Spec deviations

- **`team api --cwd` flag does not exist.** The bridge sets
  `exec.Cmd.Dir = session.cwd` on every shell-out. Functionally
  equivalent.
- **`omx team` requires a TTY.** Handled via `launch.mode` — see above.
  `omc team` does not require a TTY (verified via sanity test).

## Tests

```
go test ./...
```

Tests cover: whitelist filtering (`internal/bridge`), envelope parsing
+ capability flags + stderr noise filter (`internal/teamcli/{omx,omc}`),
shared launch argv shape (`internal/teamcli`), config provider routing
+ deprecated alias fallback (`internal/config`), TG update dedupe +
transactional processing + KV store (`internal/store`). End-to-end
acceptance criteria require a live TG bot token plus a running team and
are manual.

## Not in v0.1

Multi-session, VPS deploy, webhook mode, Linear/GitHub integration, TG
inline-keyboard approvals, attachments, voice, `/cancel` semantics,
metrics, rich formatting, hot config reload (SIGHUP / file-watch),
`/cleanup` or any TG-exposed team-cleanup path, inbound exactly-once
semantics, an event-stream substitute for omc beyond simple polling
(reading omc's event log file, fsnotify on `.omc/state/team/<team>/`,
or a future omc await-event endpoint), capability auto-detection
against the running team-cli version.
