# admiral install guide (Ubuntu 24.04)

End-to-end install for `admiral-autopilot` on a fresh Ubuntu 24.04
server. The TG bridge (`admiral`) is built alongside, but this guide is
focused on the autopilot daemon.

> Only **Ubuntu 24.04 (amd64)** is officially supported. Other distros
> may work; install.sh warns rather than refuses on mismatch.

## What you'll end up with

A long-running daemon that:

1. Listens for Linear "Agent session events" on `:8787` (HTTP webhook)
2. Spins up a per-issue git worktree forked from `origin/<base>`
3. Runs `claude -p` headless inside the worktree to do the work
4. Verifies a PR was opened (and falls back to opening one if claude
   didn't), then posts the PR URL into the Linear agent thread

## Prerequisite checklist

| # | Item | Why |
|---|---|---|
| 1 | Go 1.22+, git, gh, Claude Code CLI | build + runtime deps |
| 2 | [oh-my-claudecode](https://github.com/Yeachan-Heo/oh-my-claudecode) | provides skills `claude -p` invokes |
| 3 | [claude-harness-pack](https://github.com/virgil2019/claude-harness-pack) | workflow + `gh-accounts.yml` routing |
| 4 | `gh auth login` | admiral's runtime user must be authenticated for the target repo's GitHub account |
| 5 | admiral binary + config.yaml | this repo |
| 6 | Public-facing webhook URL | Linear must reach `:8787` (use a reverse proxy for TLS) |

## Step 1 — System packages

```bash
sudo apt update
sudo apt install -y git curl ca-certificates build-essential
```

### Go 1.22+

Pick **one**:

```bash
# Option A — official tarball (recommended; apt may lag)
curl -LO https://go.dev/dl/go1.22.4.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.4.linux-amd64.tar.gz
echo 'export PATH=/usr/local/go/bin:$PATH' >> ~/.profile
source ~/.profile

# Option B — Ubuntu apt
sudo apt install -y golang-go
```

Verify: `go version` should report 1.22 or newer.

### gh CLI

```bash
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
    | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
sudo chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
    | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null
sudo apt update && sudo apt install -y gh
```

(Authoritative source: <https://cli.github.com/>.)

### Claude Code CLI

Follow the official installer:
<https://docs.claude.com/en/docs/claude-code/quickstart>

Verify: `claude --version`.

## Step 2 — oh-my-claudecode

OMC ships skills like `autopilot` that admiral's prompt can prepend
(via `autopilot.autopilot_skill` in config.yaml). Install per upstream:

→ <https://github.com/Yeachan-Heo/oh-my-claudecode>

After install, OMC slash commands (e.g. `/oh-my-claudecode:autopilot`)
should be available inside Claude Code.

## Step 3 — claude-harness-pack

harness-pack provides the workflow skills (`start-task`, `finish-task`,
…) and the `~/.claude/gh-accounts.yml` mapping convention.

→ <https://github.com/virgil2019/claude-harness-pack>

Per its README, register the marketplace in `~/.claude/settings.json`:

```json
{
  "extraKnownMarketplaces": {
    "claude-harness-pack": {
      "source": { "source": "github", "repo": "virgil2019/claude-harness-pack" }
    }
  }
}
```

Then in a Claude Code REPL:

```
/plugin install harness-starter@claude-harness-pack
```

## Step 4 — gh auth + accounts file

Auth the GitHub account that owns the target repo:

```bash
gh auth login
gh auth status     # confirm the right account is active
```

Optionally, `~/.claude/gh-accounts.yml` for multi-account routing:

```yaml
accounts:
  - alias: virgil           # SSH host alias from ~/.ssh/config
    gh_user: virgil2019     # github.com username
  - alias: workhost
    gh_user: yourcompany
```

(harness-pack's `finish-task` uses this; admiral itself relies on
`gh auth status` matching the repo's remote at runtime.)

## Step 5 — Build and install admiral

```bash
git clone git@github.com:virgil2019/admiral.git
cd admiral
bash scripts/install.sh
```

Defaults to `~/.local/bin/`. Variants:

```bash
bash scripts/install.sh --prefix=/usr/local   # system-wide
bash scripts/install.sh --systemd             # also install systemd unit
bash scripts/install.sh --help                # all flags
```

Verify:

```bash
admiral-autopilot --help
```

If `~/.local/bin` is not on PATH:

```bash
echo 'export PATH=$HOME/.local/bin:$PATH' >> ~/.bashrc
source ~/.bashrc
```

## Step 6 — config.yaml

```bash
mkdir -p ~/.config/admiral
cp config.example.yaml ~/.config/admiral/config.yaml
$EDITOR ~/.config/admiral/config.yaml
```

For autopilot-only deploys, the bridge keys (`bot_token`,
`allowed_tg_user_ids`, `session.*`, `launch.*`) are ignored. Required
keys:

```yaml
linear:
  api_token: "lin_oauth_..."         # OAuth access token from your Linear app install
  webhook_secret: "lin_wh_..."       # signing secret from the webhook config

autopilot:
  listen_addr: ":8787"
  repo_dir: "/home/youruser/code/your-repo"
  base_branch: "main"
  # Optional: prefix the prompt with /<skill>
  # autopilot_skill: "oh-my-claudecode:autopilot"

storage:
  sqlite_path: "~/.local/share/admiral/autopilot.db"

logging:
  level: "info"
  file: "~/.local/share/admiral/autopilot.log"
```

## Step 7 — Linear webhook

In your Linear app's Webhooks tab, add:

- **URL**: `https://<your-public-host>/webhook`
- **Subscribe to**: Agent session events (only)
- **Signing secret**: copy → paste into `linear.webhook_secret`

Behind a reverse proxy (recommended; terminates TLS):

```nginx
# /etc/nginx/sites-available/admiral
server {
    listen 443 ssl http2;
    server_name your-public-host;
    # ... ssl_certificate, ssl_certificate_key ...

    location /webhook {
        proxy_pass http://127.0.0.1:8787;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_read_timeout 60s;
    }
}
```

## Step 8 (optional) — systemd unit

If you ran `install.sh --systemd`, the unit is at
`/etc/systemd/system/admiral-autopilot.service`. Otherwise:

```bash
sudo install -m 0644 systemd/admiral-autopilot.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable admiral-autopilot.service
```

Edit the unit to fill in `User=`, `Group=`, `WorkingDirectory=`, and
adjust the `ExecStart=` path if you used `--prefix=$HOME/.local`:

```ini
[Service]
User=admiral
Group=admiral
WorkingDirectory=/home/admiral
ExecStart=/usr/local/bin/admiral-autopilot --config /etc/admiral/config.yaml
```

Stage the system-wide config (it holds Linear secrets — restrict perms):

```bash
sudo mkdir -p /etc/admiral
sudo cp ~/.config/admiral/config.yaml /etc/admiral/config.yaml
sudo chown admiral:admiral /etc/admiral/config.yaml
sudo chmod 0640 /etc/admiral/config.yaml
```

Start:

```bash
sudo systemctl start admiral-autopilot
sudo systemctl status admiral-autopilot
journalctl -u admiral-autopilot -f
```

## Step 9 — Smoke test

Healthz:

```bash
curl http://127.0.0.1:8787/healthz       # → ok
```

In Linear, assign an issue to the agent (or @mention it in a comment).
The agent thread should show: 💭 thought → ⚡ action(s) → ✅ response
with the PR URL.

## Troubleshooting

- **`linear.api_token is required` on startup**: `--config` path wrong
  or required keys missing.
- **Webhook 401**: signing secret mismatch. Re-copy from Linear's
  Webhooks tab.
- **`claude` prompts for "trust this folder?" on first run**: SSH in,
  `sudo -u admiral -i`, run `claude` once interactively in the target
  repo directory and accept the trust dialog. After that it persists.
- **`gh` errors inside admiral but works for you interactively**:
  admiral's runtime user has no auth.
  `sudo -u admiral gh auth status` and re-login as needed.
- **Worktree paths exist from a prior failed run**: admiral cleans up
  on the next run for the same issue (`git worktree remove --force` +
  `git branch -D`). Manual cleanup if needed:
  `git -C <repo_dir> worktree prune`.
