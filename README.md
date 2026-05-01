# admiral

Two Go binaries in one repo:

- **`admiral`** — TG bridge. Bridges a single Telegram chat to a single
  team-cli team (`omx` or `omc`) via `<bin> team api`. Mac-local MVP.
  This is what the bulk of this README covers.
- **`admiral-autopilot`** (v0.5) — Linear-driven autopilot. Listens for
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

## admiral-autopilot (v0.5, Linear-driven)

A separate binary that picks up Linear issues assigned to it (assign or
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
     events"** (and only that — Issue events are not used).
2. **Run the OAuth flow** using the built-in CLI:
   ```
   admiral-oauth login --config ~/.config/admiral/config.yaml
   ```
   This opens your browser to Linear's authorization page. After you
   approve, tokens are automatically stored in SQLite. No external
   scripts needed.
3. **Save secrets** somewhere safe (1Password, etc.). admiral expects:
   - `lin_oauth_*` access token → `linear.api_token` (already stored via
     step 2; you can also set `linear.api_token` in config as a fallback)
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
  AgentSessionEvent for personal API keys.

### Token expiry

OAuth access tokens expire. When an API call receives a 401, admiral
automatically refreshes the token using the stored `refresh_token` and
retries the request once. If the refresh fails (e.g. the refresh token
is also expired or revoked), admiral fails fast with an error — no
infinite retry loop. If `bot_token` and `allowed_tg_user_ids` are
configured, admiral also sends a Telegram alert so you know to re-run
`admiral-oauth login`.

### Routing (project_id, multi-repo)

Issues are routed by their Linear **project UUID** — not team ID. Each
project maps to exactly one repo in `autopilot.repos`:

```yaml
autopilot:
  repos:
    - project_id: "00000000-0000-0000-0000-000000000000"  # Linear project UUID
      project_name: "Admiral"
      repo_dir: "/path/to/your-repo"
      base_branch: "main"
```

Finding a project UUID: Linear's web URL only exposes a slug. Use the
GraphQL API (or the Linear MCP `list_projects` tool):
```
query { projects { nodes { id name } } }
```

An issue without a project assigned is **rejected** at pre-flight. If
one product spans multiple repos, create one Linear project per repo.

### Run

```yaml
# config.yaml — minimal autopilot section
linear:
  api_token: "lin_oauth_..."
  webhook_secret: "lin_wh_..."
autopilot:
  listen_addr: ":8787"        # webhook receiver (Linear → this port)
  admin_listen_addr: "127.0.0.1:8788"  # admin API/UI (localhost only by default)
  admin_token: "change-me"    # static token for admin auth (Bearer or cookie)
  repo_dir: "/path/to/your/repo"
  repos:
    - project_id: "..."
      project_name: "MyProject"
      repo_dir: "/path/to/my-repo"
      base_branch: "main"
  # bot_token and allowed_tg_user_ids enable Telegram alerts when OAuth breaks:
  # bot_token: "123456:ABC-..."
  # allowed_tg_user_ids: [111222333]
storage:
  sqlite_path: "~/.local/share/admiral/autopilot.db"
logging:
  level: "info"
```

```
# expose port 8787 publicly so Linear can reach the webhook
cloudflared tunnel --url http://localhost:8787   # or ngrok http 8787

# update the Linear app's webhook URL to match the tunnel

# run the daemon
go run ./cmd/admiral-autopilot --config ~/.config/admiral/config.yaml
```

In Linear, on a toy issue, either:
- Assign the issue to admiral, or
- `@mention` admiral in a comment

The agent thread should show: 💭 thought → ⚡ action(s) → ✅ response with
PR URL. The PR opens on `repo_dir`'s GitHub remote (admiral expects
`gh auth login` to have been run for the right account).

### Admin API (read + write)

The admin server binds to `autopilot.admin_listen_addr` (default
`127.0.0.1:8788`) and is authenticated with the static token in
`autopilot.admin_token`. Pass the token as a Bearer header:

```
curl -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8788/admin/health
```

Or visit `/admin/ui/login` in a browser and log in — a cookie is set.

#### Read-only endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/admin/health` | `{"ok": bool, "uptime_s": int, "db_ok": bool, "linear_token_valid": bool}` |
| GET | `/admin/load` | `{"workers": int, "pending_events": int, "processing_events": int, "in_flight_jobs": int}` |
| GET | `/admin/repos` | `[{project_id, project_name, repo_dir, base_branch, enabled}]` |
| GET | `/admin/jobs` | `?status=&team_id=&since=&limit=` — list jobs |
| GET | `/admin/jobs/<session_id>` | single job detail |
| GET | `/admin/jobs/<session_id>/stream` | `claude -p` stream-json log file |

#### Write endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/admin/repos` | add a repo `{"team_id","team_name","repo_dir","base_branch?"}` |
| PATCH | `/admin/repos/<project_id>` | update `{"repo_dir?","base_branch?","enabled?"}` |
| DELETE | `/admin/repos/<project_id>` | remove a repo |
| POST | `/admin/repos/<project_id>/check_gh` | verify `gh auth status` for the repo's remote |
| POST | `/admin/repos/<project_id>/test_clone` | verify git fetch works against the remote |

#### Admin web UI

The admin UI is an htmx console at `/admin/ui/`:

| URL | Purpose |
|-----|---------|
| `/admin/ui/` | Dashboard: load card + recent jobs |
| `/admin/ui/login` | Login form |
| `/admin/ui/repos` | Repo management |
| `/admin/ui/jobs/` | Jobs list with filters |
| `/admin/ui/jobs/<session_id>` | Single job detail + stream log |

### Operations

#### OAuth failure (circuit breaker + Telegram alert)

When the Linear API starts returning 401s and token refresh also fails,
the worker short-circuits: it stops dispatching new issues and logs a
permanent-failure event. If `bot_token` and `allowed_tg_user_ids` are
set, admiral sends one Telegram message per outage to the first user in
`allowed_tg_user_ids`:

```
⚠️ admiral OAuth has stopped working: <reason>

Run `admiral-oauth` to re-authorize. N webhook event(s) queued waiting.
```

After re-running `admiral-oauth login`, admiral automatically detects the
new token on the next health check and resumes.

#### Pre-flight short-circuits

Before running `claude -p`, admiral checks the Linear issue state:

- **Already done** — if the issue was closed/resolved before pre-flight,
  the job is marked `done` immediately with no worktree created.
- **Branch already merged** — if a prior run opened a PR that was already
  merged, the worktree is archived (not deleted) and the job is marked
  `done`.
- **Prior follow-up** — if a prior session ended with a `done` state and
  a new `AgentSessionEvent.prompted` (follow-up) arrives, admiral
  dispatches a `resume` reply on the existing job rather than starting
  fresh.

Jobs in these states appear in the admin UI (`/admin/ui/jobs/`) with their
state badge so you can distinguish a successful run from a short-circuit.

#### Worktree layout

```
<repo_dir>/
  .worktrees/               # worktree_root (default)
    <issue-id>-<session>/   # one worktree per job
  .worktrees-archive/       # archived (merged/failed) worktrees land here
```

Add `.worktrees-archive/` to your `.gitignore` to keep completed worktrees
out of git status:

```
echo ".worktrees-archive/" >> /path/to/your/repo/.gitignore
```

### What's not in v0.5

- Multi-issue parallel execution (single-flight: a second assignment
  during a run gets a "busy" reply and is dropped)
- PR review comment → admiral feedback loop
- Crash recovery / orphan job replay (jobs stay in whatever state they
  were in when the process died)
- Workflow state changes via `issueUpdate` (the agent thread carries
  visible progress instead — board view stays static)

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
[admiral-autopilot (v0.5)](#admiral-autopilot-v05-linear-driven) above.

<!-- parallel-test marker: 2026-04-30T03:30 -->
