#!/usr/bin/env bash
# agentrun installer — works either via `curl | sh` or `./install.sh` from a clone.
#
#   curl -fsSL https://raw.githubusercontent.com/AI4Everyonee/agentrun/main/install.sh | sh
#   # or:
#   git clone https://github.com/AI4Everyonee/agentrun && cd agentrun && ./install.sh
#
# Flags:
#   --local-build   build the binary from source instead of downloading a release
#                   (requires Go; only meaningful when run from a clone)
#   --version vX    install a specific release tag (default: latest)
#   --reinstall     overwrite an existing config.env (default: preserve)
#
# Idempotent. Safe to re-run. Run uninstall.sh to remove.

set -euo pipefail

# ─── Config ──────────────────────────────────────────────────────────────────
REPO="AI4Everyonee/agentrun"
CONTAINER_NAME="agentrun-postgres"
PG_PORT="5433"
PG_VOLUME="agentrun_pgdata"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/agentrun"
CONFIG_FILE="$CONFIG_DIR/config.env"
LOG_DIR="$HOME/Library/Logs"   # macOS only; Linux uses journald

LOCAL_BUILD=0
VERSION=""
REINSTALL=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --local-build) LOCAL_BUILD=1; shift ;;
        --version)     VERSION="$2"; shift 2 ;;
        --reinstall)   REINSTALL=1; shift ;;
        -h|--help)
            sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *) echo "unknown flag: $1" >&2; exit 2 ;;
    esac
done

# Detect whether we have the source tree locally (clone) or are running solo (curl|sh).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
HAS_LOCAL_TREE=0
[[ -f "$SCRIPT_DIR/go.mod" && -d "$SCRIPT_DIR/packaging" ]] && HAS_LOCAL_TREE=1

# ─── Helpers ─────────────────────────────────────────────────────────────────
say()  { printf "▸ %s\n" "$*"; }
warn() { printf "! %s\n" "$*" >&2; }
die()  { printf "✗ %s\n" "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 not found in PATH. Install it and re-run."; }

OS_KERNEL="$(uname -s)"
ARCH_RAW="$(uname -m)"
case "$OS_KERNEL" in
    Darwin) OS="darwin" ;;
    Linux)  OS="linux"  ;;
    *) die "unsupported OS: $OS_KERNEL (macOS and Linux only)" ;;
esac
case "$ARCH_RAW" in
    arm64|aarch64) ARCH="arm64" ;;
    x86_64|amd64)  ARCH="amd64" ;;
    *) die "unsupported arch: $ARCH_RAW" ;;
esac
say "platform: $OS/$ARCH"

# ─── Docker + Postgres ───────────────────────────────────────────────────────
need docker
docker info >/dev/null 2>&1 || die "docker daemon is not running. Start Docker Desktop (or dockerd) and re-run."

ensure_postgres() {
    local existing
    existing="$(docker ps -a --filter "name=^/${CONTAINER_NAME}$" --format '{{.Names}}' || true)"
    if [[ -z "$existing" ]]; then
        say "creating Postgres container ($CONTAINER_NAME) on port $PG_PORT…"
        docker run -d \
            --name "$CONTAINER_NAME" \
            --restart unless-stopped \
            -p "${PG_PORT}:5432" \
            -e POSTGRES_USER=agentrun \
            -e POSTGRES_PASSWORD=agentrun \
            -e POSTGRES_DB=agentrun \
            -v "${PG_VOLUME}:/var/lib/postgresql/data" \
            pgvector/pgvector:pg16 >/dev/null
    else
        say "Postgres container already exists; ensuring --restart unless-stopped…"
        docker update --restart unless-stopped "$CONTAINER_NAME" >/dev/null
        local running
        running="$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null || echo false)"
        [[ "$running" != "true" ]] && docker start "$CONTAINER_NAME" >/dev/null
    fi
    say "waiting for Postgres to be ready…"
    local tries=0
    until docker exec "$CONTAINER_NAME" pg_isready -U agentrun -d agentrun >/dev/null 2>&1; do
        tries=$((tries+1))
        (( tries > 30 )) && die "Postgres did not become ready within 30s"
        sleep 1
    done
}
ensure_postgres

# ─── Binary ──────────────────────────────────────────────────────────────────
mkdir -p "$BIN_DIR"
BIN_PATH="$BIN_DIR/agentrun"

build_locally() {
    need go
    say "building agentrun from source ($SCRIPT_DIR)…"
    ( cd "$SCRIPT_DIR" && go build -o "$BIN_PATH" . )
}

download_release() {
    need curl
    if [[ -z "$VERSION" ]]; then
        say "resolving latest release of $REPO…"
        VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
            | grep -E '"tag_name"' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
        [[ -z "$VERSION" ]] && die "could not resolve latest release. Pass --version vX.Y.Z, or use --local-build if you have a clone."
    fi
    local url="https://github.com/$REPO/releases/download/$VERSION/agentrun-${OS}-${ARCH}"
    say "downloading $url"
    curl -fsSL --retry 3 -o "$BIN_PATH.tmp" "$url" \
        || die "download failed. Verify the release exists, or use --local-build."
    mv "$BIN_PATH.tmp" "$BIN_PATH"
}

if (( LOCAL_BUILD )); then
    (( HAS_LOCAL_TREE )) || die "--local-build requires running install.sh from a clone of the repo."
    build_locally
else
    download_release
fi
chmod +x "$BIN_PATH"
say "installed binary → $BIN_PATH"

case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *) warn "$BIN_DIR is not on your PATH. Add this to your shell rc:"
       warn "    export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
esac

# ─── Config file ─────────────────────────────────────────────────────────────
mkdir -p "$CONFIG_DIR"
if [[ -f "$CONFIG_FILE" && $REINSTALL -eq 0 ]]; then
    say "preserving existing config → $CONFIG_FILE"
else
    detect_user() {
        local u
        u="$(git config --global user.email 2>/dev/null || true)"
        [[ -z "$u" ]] && u="${USER:-$(whoami)}@$(hostname -s 2>/dev/null || echo unknown)"
        printf '%s' "$u"
    }
    cat > "$CONFIG_FILE" <<EOF
# agentrun config — edit and restart the service to apply.
# macOS:   launchctl kickstart -k gui/\$UID/com.agentrun.watcher
# Linux:   systemctl --user restart agentrun

DATABASE_URL=postgres://agentrun:agentrun@localhost:${PG_PORT}/agentrun
AGENTRUN_USER=$(detect_user)
EOF
    chmod 600 "$CONFIG_FILE"
    say "wrote config → $CONFIG_FILE"
fi

# ─── Service ─────────────────────────────────────────────────────────────────
# Render a template: read template, sed-substitute placeholders.
render_template() {
    local src="$1" dst="$2"
    [[ -f "$src" ]] || die "template missing: $src"
    sed \
        -e "s|__BIN__|$BIN_PATH|g" \
        -e "s|__HOME__|$HOME|g" \
        -e "s|__CONFIG__|$CONFIG_FILE|g" \
        -e "s|__LOG_OUT__|$LOG_DIR/agentrun.out.log|g" \
        -e "s|__LOG_ERR__|$LOG_DIR/agentrun.err.log|g" \
        "$src" > "$dst"
}

template_path() {
    local name="$1"
    if (( HAS_LOCAL_TREE )); then
        printf '%s/packaging/%s' "$SCRIPT_DIR" "$name"
    else
        local tmp; tmp="$(mktemp)"
        curl -fsSL --retry 3 -o "$tmp" \
            "https://raw.githubusercontent.com/$REPO/main/packaging/$name" \
            || die "could not download packaging template $name"
        printf '%s' "$tmp"
    fi
}

# Stop any existing watcher (other than systemd/launchd-managed ones we're about to take over).
existing_pid="$(pgrep -f 'agentrun watch' || true)"
if [[ -n "$existing_pid" ]]; then
    say "stopping existing watcher (pid $existing_pid)…"
    pkill -f 'agentrun watch' || true
    sleep 1
fi

if [[ "$OS" == "darwin" ]]; then
    mkdir -p "$LOG_DIR"
    PLIST_SRC="$(template_path com.agentrun.watcher.plist.tmpl)"
    PLIST_DST="$HOME/Library/LaunchAgents/com.agentrun.watcher.plist"
    mkdir -p "$(dirname "$PLIST_DST")"
    render_template "$PLIST_SRC" "$PLIST_DST"
    # Reload: unload silently if already loaded, then load.
    launchctl bootout "gui/$UID/com.agentrun.watcher" 2>/dev/null || true
    launchctl bootstrap "gui/$UID" "$PLIST_DST"
    say "loaded launchd service → $PLIST_DST"
    say "logs: $LOG_DIR/agentrun.{out,err}.log"
else
    UNIT_SRC="$(template_path agentrun.service.tmpl)"
    UNIT_DST="$HOME/.config/systemd/user/agentrun.service"
    mkdir -p "$(dirname "$UNIT_DST")"
    render_template "$UNIT_SRC" "$UNIT_DST"
    systemctl --user daemon-reload
    systemctl --user enable --now agentrun.service
    # Keep the service running after logout.
    if command -v loginctl >/dev/null 2>&1; then
        loginctl enable-linger "$USER" 2>/dev/null || \
            warn "could not enable-linger; the watcher will stop when you log out. Try: sudo loginctl enable-linger $USER"
    fi
    say "loaded systemd service → $UNIT_DST"
    say "logs: journalctl --user -u agentrun -f"
fi

# ─── Verify ──────────────────────────────────────────────────────────────────
sleep 2
say "status:"
"$BIN_PATH" status || warn "status reported unhealthy — check logs (above)."
say "done. Edit $CONFIG_FILE to tweak settings; re-run install.sh to upgrade."
