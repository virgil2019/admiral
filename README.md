# admiral

<!-- GEO-8: test session marker, no behavior change -->
Two Go binaries in one repo:

- **`admiral`** — TG bridge. Bridges a single Telegram chat to a single
  team-cli team (`omx` or `omc`) via `<bin> team api`. Mac-local MVP.
  This is what the bulk of this README covers.
- **`admiral-autopilot`** (v0.3) — Linear-driven autopilot. Listens for
  Linear `AgentSessionEvent` webhooks (assign / @mention), creates a
  worktree, runs `claude -p`, opens a PR, posts progress back into the
  Linear agent thread. See [admiral-autopilot setup](#admiral-autopilot-v03-linear-driven)
  for the dedicated section.

Repo location: `/Users/georgehuang/Program/george/admiral` (repo root —
`go run ./cmd/admiral` or `go run ./cmd/admiral-autopilot` from here).

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

## admiral-autopilot (v0.3, Linear-driven)

A separate binary that picks up Linear issues delegated to it (assign or
`@mention` an Agent app), runs `claude -p` in a per-issue worktree, opens
a PR, and reports progress back into the Linear agent thread via the
Linear Agent SDK.

> **Deploying on Ubuntu 24.04?** See [docs/install.md](docs/install.md)
> for the end-to-end install flow (system deps, OMC, harness-pack,
> `gh auth`, `scripts/install.sh`, config, Linear webhook, optional
> systemd unit). The subsections below are the dev-mode reference; the
> install doc is the canonical production path.

### One-time Linear setup

This is **manual** — the Linear OAuth handshake requires a workspace
admin to approve the install in a browser, so admiral can't bootstrap
itself. Do this once per workspace:

1. **Create an OAuth app** in Linear → Settings → API → "Create new" under
   OAuth applications. Fill the form, then importantly:
   - Toggle **Webhooks: ON** (otherwise no events ship + no signing
     secret is generated)
   - After Create, edit the app's **Webhooks** section: set URL to
     `https://<your-tunnel>/webhook`, subscribe to **"Agent session
     events"** (and only that — Issue events are not used in v0.3).
2. **Run the OAuth flow** to get a `lin_oauth_*` access token. The
   simplest path is to clone Linear's official agent demo or copy the
   minimal `oauth-callback.ts` script (see
   `/Users/georgehuang/Program/test/ai/team/linear/src/oauth-callback.ts`
   in this dev's tree for a working reference).

   Required ingredients for the flow:
   - Authorization URL: `https://linear.app/oauth/authorize`
   - Required query params: `response_type=code`, `client_id=...`,
     `redirect_uri=http://127.0.0.1:8080/callback`,
     `scope=read,write,app:mentionable,app:assignable`,
     `actor=app`, `prompt=consent`, plus a CSRF `state`
   - Token exchange: `POST https://api.linear.app/oauth/token` with
     `code`, `redirect_uri`, `client_id`, `client_secret`,
     `grant_type=authorization_code`
   - Output: `access_token` (this is the value you paste into
     `linear.api_token`), plus `refresh_token` for future renewal.
3. **Save secrets** somewhere safe (1Password, etc.). admiral expects:
   - `lin_oauth_*` access token → `linear.api_token`
   - Webhook signing secret (Linear app's Webhooks section) →
     `linear.webhook_secret`

### Why an OAuth access token (not just client_secret or a Personal API key)

- `client_id` + `client_secret` identify the **app** to Linear's OAuth
  endpoint. They don't carry workspace context, install actor type, or
  scopes — Linear's API rejects them as auth.
- The `lin_oauth_*` access token is the only credential that simultaneously
  encodes (workspace × `actor=app` (Agent) × `app:mentionable` /
  `app:assignable` scopes). admiral needs all three for the Agent SDK to
  route AgentSessionEvent to it.
- A `lin_api_*` Personal API key is workspace-scoped but acts as **you**,
  not as a separate Agent. Mentions/assignments aren't routed via
  AgentSessionEvent for personal API keys. v0.3 is built around the
  Agent SDK, so a personal key won't drive it.

### Token expiry

OAuth access tokens expire (Linear's lifetime is typically months but
not eternal). v0.3 does NOT auto-refresh — when the token expires:

- API calls return 401
- Re-run the OAuth flow once, paste the new `access_token` into
  `linear.api_token`, restart the daemon

A `LINEAR_REFRESH_TOKEN` is also returned by the OAuth flow; auto-renewal
using it is on the v0.4 list.

### Run

```
# minimal config — see config.example.yaml for the full annotated form
cat > ~/.config/admiral/config.yaml <<'YAML'
linear:
  api_token: "lin_oauth_..."
  webhook_secret: "lin_wh_..."
autopilot:
  listen_addr: ":8787"
  repo_dir: "/path/to/your/repo"
storage:
  sqlite_path: "~/.local/share/admiral/autopilot.db"
logging:
  level: "info"
YAML

# expose port 8787 publicly so Linear can reach the webhook
cloudflared tunnel --url http://localhost:8787   # or ngrok http 8787

# (update the Linear app's webhook URL to match the tunnel)

# run the daemon
go run ./cmd/admiral-autopilot --config ~/.config/admiral/config.yaml
```

In Linear, on a toy issue, either:
- Assign the issue to admiral, or
- `@mention` admiral in a comment

The agent thread should show: 💭 thought → ⚡ action(s) → ✅ response with
PR URL. The PR opens on `repo_dir`'s GitHub remote (admiral expects
`gh auth login` to have been run for the right account).

### What's not in v0.3

- Follow-up messages in the agent thread (`AgentSessionEvent.prompted`)
  — admiral posts a stub "v0.3 doesn't handle follow-ups yet" reply
- Auto-refresh of expired OAuth tokens via `refresh_token`
- Multi-issue parallel execution (single-flight: a second assignment
  during a run gets a "busy" reply and is dropped)
- Workflow state changes via `issueUpdate` (the agent thread carries
  visible progress instead — board view stays static)
- PR review comment → admiral feedback loop
- Crash recovery / orphan job replay

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

## Not in v0.1 (TG bridge scope)

Multi-session, VPS deploy, webhook mode, TG inline-keyboard approvals,
attachments, voice, `/cancel` semantics, metrics, rich formatting,
hot config reload (SIGHUP / file-watch), `/cleanup` or any TG-exposed
team-cleanup path, inbound exactly-once semantics, an event-stream
substitute for omc beyond simple polling (reading omc's event log
file, fsnotify on `.omc/state/team/<team>/`, or a future omc await-event
endpoint), capability auto-detection against the running team-cli
version.

Linear/GitHub integration moved to its own binary — see
[admiral-autopilot (v0.3)](#admiral-autopilot-v03-linear-driven) above.
