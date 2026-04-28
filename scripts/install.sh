#!/usr/bin/env bash
# install.sh — build admiral binaries and install on Ubuntu 24.04.
#
# This script ONLY handles the admiral side. Prereqs you install
# manually (see docs/install.md):
#   - Go 1.22+, git, gh, Claude Code CLI
#   - oh-my-claudecode (https://github.com/Yeachan-Heo/oh-my-claudecode)
#   - claude-harness-pack (https://github.com/virgil2019/claude-harness-pack)
#   - gh auth login + ~/.claude/gh-accounts.yml
#
# After this script runs, you still need to populate
# ~/.config/admiral/config.yaml and configure the Linear webhook.

set -euo pipefail

if [ -t 1 ]; then
    C_RED=$'\033[31m'
    C_GREEN=$'\033[32m'
    C_YELLOW=$'\033[33m'
    C_BLUE=$'\033[34m'
    C_RESET=$'\033[0m'
else
    C_RED=
    C_GREEN=
    C_YELLOW=
    C_BLUE=
    C_RESET=
fi

log_info()  { printf '%s[info]%s %s\n' "$C_BLUE"   "$C_RESET" "$*"; }
log_warn()  { printf '%s[warn]%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
log_error() { printf '%s[err ]%s %s\n' "$C_RED"    "$C_RESET" "$*" >&2; }
log_ok()    { printf '%s[ ok ]%s %s\n' "$C_GREEN"  "$C_RESET" "$*"; }

PREFIX="${HOME}/.local"
INSTALL_SYSTEMD=0
SKIP_BUILD=0

usage() {
    cat <<EOF
Usage: $(basename "$0") [options]

Build and install admiral binaries on Ubuntu 24.04.

Options:
  --prefix=PATH    Install root for binaries (default: \$HOME/.local).
                   Binaries land in <prefix>/bin/. Use /usr/local for a
                   system-wide install (sudo will be requested for that).
  --systemd        Also install the systemd unit to /etc/systemd/system/,
                   reload, and enable it (does NOT auto-start; you must
                   populate the config first — see docs/install.md).
  --skip-build     Reuse existing bin/admiral{,-autopilot} instead of
                   re-running 'go build'.
  -h, --help       Show this help.

Run from anywhere; script resolves the repo root via its own location.

Full deploy flow (config, webhook, prereqs): docs/install.md.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --prefix)       PREFIX="$2"; shift 2 ;;
        --prefix=*)     PREFIX="${1#--prefix=}"; shift ;;
        --systemd)      INSTALL_SYSTEMD=1; shift ;;
        --skip-build)   SKIP_BUILD=1; shift ;;
        -h|--help)      usage; exit 0 ;;
        *)              log_error "unknown flag: $1"; usage; exit 2 ;;
    esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

# --- platform check (warn-only) ---
if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    if [ "${ID:-}" != "ubuntu" ] || [ "${VERSION_ID:-}" != "24.04" ]; then
        log_warn "expected Ubuntu 24.04, got ${PRETTY_NAME:-unknown}; proceeding anyway"
    fi
else
    log_warn "/etc/os-release missing; cannot verify Ubuntu 24.04"
fi

# --- dependency presence ---
need() {
    if ! command -v "$1" >/dev/null 2>&1; then
        log_error "$1 not found in PATH; $2"
        return 1
    fi
}

dep_fail=0
need git "install with: sudo apt install -y git"                                || dep_fail=1
need gh  "install instructions: https://cli.github.com/ (Ubuntu apt repo)"      || dep_fail=1
need go  "install Go 1.22+: https://go.dev/dl (or 'sudo apt install golang-go')" || dep_fail=1
[ "$dep_fail" -eq 0 ] || exit 1

# --- go version >= 1.22 ---
if go_ver_full="$(go env GOVERSION 2>/dev/null)"; then
    go_ver="${go_ver_full#go}"
else
    go_ver="$(go version | awk '{print $3}')"
    go_ver="${go_ver#go}"
fi
go_maj="${go_ver%%.*}"
go_rest="${go_ver#*.}"
go_min="${go_rest%%.*}"
go_min="${go_min%%[!0-9]*}"  # drop pre-release suffix like 'rc1'
if [ "${go_maj:-0}" -lt 1 ] || { [ "${go_maj:-0}" -eq 1 ] && [ "${go_min:-0}" -lt 22 ]; }; then
    log_error "Go ${go_ver} found; admiral needs >= 1.22"
    exit 1
fi
log_ok "go ${go_ver}"

# --- build ---
mkdir -p bin
if [ "$SKIP_BUILD" -eq 0 ]; then
    log_info "building admiral..."
    go build -o bin/admiral ./cmd/admiral
    log_info "building admiral-autopilot..."
    go build -o bin/admiral-autopilot ./cmd/admiral-autopilot
    log_ok "built bin/admiral and bin/admiral-autopilot"
else
    for f in bin/admiral bin/admiral-autopilot; do
        [ -x "$f" ] || { log_error "$f missing; cannot --skip-build"; exit 1; }
    done
    log_ok "skip-build: reused existing binaries"
fi

# --- install binaries ---
INSTALL_BIN_DIR="$PREFIX/bin"
if [ ! -d "$INSTALL_BIN_DIR" ]; then
    mkdir -p "$INSTALL_BIN_DIR" 2>/dev/null || {
        log_warn "cannot mkdir $INSTALL_BIN_DIR; retrying with sudo"
        sudo mkdir -p "$INSTALL_BIN_DIR"
    }
fi

install_bin() {
    local src="$1" dst="$2"
    if [ -w "$(dirname "$dst")" ]; then
        install -m 0755 "$src" "$dst"
    else
        sudo install -m 0755 "$src" "$dst"
    fi
}
install_bin bin/admiral           "$INSTALL_BIN_DIR/admiral"
install_bin bin/admiral-autopilot "$INSTALL_BIN_DIR/admiral-autopilot"
log_ok "installed binaries to $INSTALL_BIN_DIR"

# --- systemd (optional) ---
if [ "$INSTALL_SYSTEMD" -eq 1 ]; then
    unit_src="$REPO_ROOT/systemd/admiral-autopilot.service"
    unit_dst="/etc/systemd/system/admiral-autopilot.service"
    if [ ! -r "$unit_src" ]; then
        log_error "unit file missing: $unit_src"
        exit 1
    fi
    if [ "$(id -u)" -ne 0 ]; then
        sudo install -m 0644 "$unit_src" "$unit_dst"
        sudo systemctl daemon-reload
        sudo systemctl enable admiral-autopilot.service
    else
        install -m 0644 "$unit_src" "$unit_dst"
        systemctl daemon-reload
        systemctl enable admiral-autopilot.service
    fi
    log_ok "systemd unit installed and enabled (NOT started)"
    cat <<EOF

systemd next steps:
  1. Edit $unit_dst — set User=, Group=, WorkingDirectory=, and the
     ExecStart= path if you used a non-default --prefix.
  2. Place config at /etc/admiral/config.yaml (or wherever ExecStart points).
  3. sudo systemctl start admiral-autopilot
  4. journalctl -u admiral-autopilot -f
EOF
fi

# --- final hint ---
cat <<EOF

${C_GREEN}done.${C_RESET}

next:
  - If $INSTALL_BIN_DIR is not on PATH:
      echo 'export PATH=$INSTALL_BIN_DIR:\$PATH' >> ~/.bashrc && source ~/.bashrc
  - Read docs/install.md for the rest of the deploy:
      * Claude Code CLI install
      * oh-my-claudecode (https://github.com/Yeachan-Heo/oh-my-claudecode)
      * claude-harness-pack (https://github.com/virgil2019/claude-harness-pack)
      * config.yaml + Linear webhook + (optional) systemd
EOF
