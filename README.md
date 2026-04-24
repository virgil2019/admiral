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

## Spec deviations

- **`omx team api --cwd` flag does not exist.** The bridge sets its own
  `exec.Cmd.Dir` to `session.cwd` instead. Functionally equivalent.
- **`omx launch` refuses to run without a TTY.** The bridge wraps launches
  with `/usr/bin/script -q /dev/null omx launch <team>` to provide a PTY.
  This is macOS-specific; Linux would need `script -q -c '...' /dev/null`.
  Documented here instead of extending scope.

## Tests

```
go test ./...
```

Included tests cover whitelist filtering (`internal/bridge`) and omx envelope
parsing (`internal/omx`). Broader integration testing against a live omx
team is manual per the acceptance criteria in the spec (task #5e39499c).

## Not in v0.1

Multi-session, VPS deploy, webhook mode, Linear/GitHub integration, TG
inline-keyboard approvals, attachments, voice, `/cancel` semantics, metrics,
rich formatting. See spec §8.
