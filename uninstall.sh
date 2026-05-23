#!/usr/bin/env bash
# agentrun uninstaller.
#
#   ./uninstall.sh           # remove service + binary, keep Postgres + data
#   ./uninstall.sh --purge   # also stop + remove the Postgres container AND its volume
#
# Safe to re-run.

set -euo pipefail

PURGE=0
[[ "${1:-}" == "--purge" ]] && PURGE=1

CONTAINER_NAME="agentrun-postgres"
PG_VOLUME="agentrun_pgdata"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/agentrun"

say()  { printf "▸ %s\n" "$*"; }
warn() { printf "! %s\n" "$*" >&2; }

# ─── Stop + remove service ───────────────────────────────────────────────────
case "$(uname -s)" in
    Darwin)
        PLIST="$HOME/Library/LaunchAgents/com.agentrun.watcher.plist"
        if [[ -f "$PLIST" ]]; then
            launchctl bootout "gui/$UID/com.agentrun.watcher" 2>/dev/null || true
            rm -f "$PLIST"
            say "removed launchd service ($PLIST)"
        else
            say "no launchd plist found; skipping"
        fi
        ;;
    Linux)
        if systemctl --user is-enabled agentrun.service >/dev/null 2>&1 \
            || systemctl --user is-active agentrun.service >/dev/null 2>&1; then
            systemctl --user disable --now agentrun.service || true
            say "disabled systemd service"
        fi
        rm -f "$HOME/.config/systemd/user/agentrun.service"
        systemctl --user daemon-reload 2>/dev/null || true
        ;;
esac

# Kill any straggler watcher.
if pgrep -f 'agentrun watch' >/dev/null 2>&1; then
    pkill -f 'agentrun watch' || true
    say "stopped running watcher process"
fi

# ─── Binary + config ─────────────────────────────────────────────────────────
if [[ -f "$BIN_DIR/agentrun" ]]; then
    rm -f "$BIN_DIR/agentrun"
    say "removed binary ($BIN_DIR/agentrun)"
fi

if (( PURGE )); then
    rm -rf "$CONFIG_DIR"
    say "removed config dir ($CONFIG_DIR)"
else
    say "preserved config dir ($CONFIG_DIR) — pass --purge to remove"
fi

# ─── Postgres (only with --purge) ────────────────────────────────────────────
if (( PURGE )); then
    if command -v docker >/dev/null 2>&1; then
        if docker ps -a --filter "name=^/${CONTAINER_NAME}$" --format '{{.Names}}' | grep -q .; then
            docker rm -f "$CONTAINER_NAME" >/dev/null
            say "removed Postgres container ($CONTAINER_NAME)"
        fi
        if docker volume ls --format '{{.Name}}' | grep -q "^${PG_VOLUME}$"; then
            docker volume rm "$PG_VOLUME" >/dev/null
            say "removed Postgres volume ($PG_VOLUME) — all ingested data is gone"
        fi
    else
        warn "docker not found; cannot clean up container/volume"
    fi
else
    say "preserved Postgres container + volume — pass --purge to remove"
fi

say "done."
