# omx-bridge v0.1

A Go daemon that bridges a single Telegram chat to a single `oh-my-codex`
(omx) team via `omx team api`. Mac-local MVP. Long-polling only, whitelist
by TG user id, 1 bot : 1 team.

## Prerequisites

- Go 1.22+
- `omx` CLI in PATH (or absolute path via `session.omx_bin_path`)
- `/usr/bin/script` (used to provide a PTY for `omx launch` — ships on macOS)
- A Telegram bot token (BotFather → `/newbot`)

## Setup

1. Copy the example config:
   ```
   mkdir -p ~/.config/omx-bridge
   cp config.example.yaml ~/.config/omx-bridge/config.yaml
   ```
2. Get a bot token from [@BotFather](https://t.me/BotFather) and paste it into
   `bot_token`.
3. Get your TG user id (message [@userinfobot](https://t.me/userinfobot)) and
   put it into `allowed_tg_user_ids`. Also fill `session.tg_chat_id` — for a
   1:1 DM chat, this is the same as your user id.
4. Fill `session.team_name`, `session.cwd`, `session.omx_bin_path`.

## Run

```
go run ./cmd/omx-bridge
```

Or with a non-default config path:

```
go run ./cmd/omx-bridge --config /path/to/config.yaml
```

## TG commands

| Command | Effect |
|---|---|
| `/start` | Launch the omx team (idempotent) |
| `/status` | Team summary |
| `/stop` | Request shutdown of team leader |
| `/help` | List commands |
| `/whoami` | Print identity |
| plain text | Forwarded to team leader as a mailbox message |

## Launch modes (config: `launch.mode`)

- **`pty`** (default on macOS). The bridge prepends `launch.pty_command`
  (default `/usr/bin/script -q /dev/null`) to `omx launch <team>` so omx
  sees a real terminal. Logs a `WARN launch.mode=pty using_script_wrapper`
  line on every `/start`.
- **`direct`** (default elsewhere). No wrapper. If omx exits with the
  "stdin is not a terminal" error, the bridge replies with instructions to
  either flip `launch.mode: pty` or pre-launch from a terminal, and keeps
  the daemon running so `/status` still works.

`launch.pty_command` is user-configurable — override if you need a different
PTY wrapper (Linux `script` has different flag semantics).

## Event loop lifecycle

The push goroutine starts when any of: `/start` succeeds; a plain-text
message is sent successfully; or at boot if a cursor is persisted in SQLite
AND `get-summary` confirms the team is still up. Boot with a non-empty
cursor but a missing team does **not** start the loop — the bridge waits
for `/start`.

## Inbound delivery guarantee

At-least-once. Dedupe key is the Telegram `update_id`. Every inbound update
is persisted to `tg_updates (update_id PRIMARY KEY)` with `INSERT OR IGNORE`
before processing, and marked processed in the same transaction as the
outbound `messages` row. A bridge crash between persist and `send-message`
is recovered on next boot — pending rows are drained before the long-poll
resumes.

## Mac sleep / wake > 23h

On every successful long-poll, the bridge persists `last_successful_poll_at`.
On boot (or after any gap), if the gap exceeds 23h (safety margin under
TG's 24h retention window), a one-time `Bridge woke after <duration>; some
older updates may have been dropped by Telegram.` is pushed. Re-push is
suppressed for 1h to avoid spam on crash loops.

## Spec deviations

- **`omx team api --cwd` flag does not exist.** The bridge sets
  `exec.Cmd.Dir = session.cwd` on every shell-out. Functionally equivalent.
- **`omx launch` requires a TTY.** Handled via `launch.mode` — see above.

## Tests

```
go test ./...
```

Tests cover: whitelist filtering (`internal/bridge`), omx envelope parsing
(`internal/omx`), TG update dedupe + transactional processing + KV store
(`internal/store`). End-to-end acceptance criteria require a live TG bot
token plus omx team and are manual (spec §7).

## Not in v0.1

Multi-session, VPS deploy, webhook mode, Linear/GitHub integration, TG
inline-keyboard approvals, attachments, voice, `/cancel` semantics, metrics,
rich formatting, hot config reload (SIGHUP / file-watch), `/cleanup` or any
TG-exposed team-cleanup path, inbound exactly-once semantics, a proper
non-TTY launch path (pending upstream omx fix or a `--no-tty` flag). See
spec §8 and product addenda on #5e39499c comment `4865398c`.
