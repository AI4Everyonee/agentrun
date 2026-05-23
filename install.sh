#!/usr/bin/env bash
# agentrun — one-shot installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/AI4Everyonee/agentrun/main/install.sh | bash
#
# Detects platform (darwin/linux × amd64/arm64), downloads the latest release
# binary from GitHub, drops it into the first writable directory among:
#   1. $AGENTRUN_INSTALL_DIR
#   2. /usr/local/bin              (if writable or sudo available)
#   3. $HOME/.local/bin            (always works without root)
#
# Then runs `agentrun install` to wire up the global Claude + Codex hooks.
#
# Environment variables:
#   AGENTRUN_INSTALL_DIR       — override install path
#   AGENTRUN_VERSION           — pin a specific release tag (e.g. v0.4.0)
#   AGENTRUN_SKIP_HOOKS=1      — install the binary but skip `agentrun install`
#   OPENAI_API_KEY             — if present, summarization works out of the box
#   GH_TOKEN | GITHUB_TOKEN    — required while the repo is PRIVATE

set -euo pipefail

REPO="AI4Everyonee/agentrun"

# ─── Pretty output helpers ────────────────────────────────────────────────────
fmt_blue=$'\033[34m'
fmt_green=$'\033[32m'
fmt_red=$'\033[31m'
fmt_dim=$'\033[2m'
fmt_reset=$'\033[0m'

step()  { printf "%s==>%s %s\n" "$fmt_blue" "$fmt_reset" "$1"; }
done_() { printf "%s✓%s %s\n" "$fmt_green" "$fmt_reset" "$1"; }
warn()  { printf "%s⚠%s %s\n" "$fmt_dim" "$fmt_reset" "$1"; }
die()   { printf "%s✗%s %s\n" "$fmt_red" "$fmt_reset" "$1" >&2; exit 1; }

# ─── 1. Detect platform ──────────────────────────────────────────────────────
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "Unsupported architecture: $arch" ;;
esac
case "$os" in
  darwin|linux) ;;
  *) die "Unsupported OS: $os (Windows is not yet supported)" ;;
esac
step "Detected platform: ${os}_${arch}"

# ─── 2. Pick the release tag ─────────────────────────────────────────────────
tag="${AGENTRUN_VERSION:-}"
if [ -z "$tag" ]; then
  step "Resolving latest release"
  api_url="https://api.github.com/repos/${REPO}/releases/latest"
  auth_header=""
  if [ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]; then
    auth_header="-H Authorization: Bearer ${GH_TOKEN:-${GITHUB_TOKEN}}"
  fi
  # shellcheck disable=SC2086
  tag=$(curl -fsSL $auth_header "$api_url" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
  if [ -z "$tag" ]; then
    die "Could not resolve latest release. If the repo is private, set GH_TOKEN."
  fi
fi
done_ "Release tag: $tag"

# ─── 3. Download the tarball ─────────────────────────────────────────────────
ver="${tag#v}"
asset="agentrun_${ver}_${os}_${arch}.tar.gz"
tarball_url="https://github.com/${REPO}/releases/download/${tag}/${asset}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

step "Downloading $asset"
auth_header_curl=()
if [ -n "${GH_TOKEN:-${GITHUB_TOKEN:-}}" ]; then
  auth_header_curl=(-H "Authorization: Bearer ${GH_TOKEN:-${GITHUB_TOKEN}}")
fi
curl -fL --progress-bar "${auth_header_curl[@]}" "$tarball_url" -o "$tmp/agentrun.tar.gz" \
  || die "Failed to download $tarball_url. If the repo is private, set GH_TOKEN."
tar -xzf "$tmp/agentrun.tar.gz" -C "$tmp"
[ -f "$tmp/agentrun" ] || die "Tarball missing the 'agentrun' binary"
chmod +x "$tmp/agentrun"

# ─── 4. Pick install directory ───────────────────────────────────────────────
choose_install_dir() {
  if [ -n "${AGENTRUN_INSTALL_DIR:-}" ]; then
    printf "%s" "$AGENTRUN_INSTALL_DIR"
    return
  fi
  if [ -w /usr/local/bin ]; then
    printf "%s" /usr/local/bin
    return
  fi
  if command -v sudo >/dev/null 2>&1 && [ "${AGENTRUN_USE_SUDO:-1}" = "1" ]; then
    if sudo -n true 2>/dev/null || [ -t 0 ]; then
      printf "%s" /usr/local/bin
      return
    fi
  fi
  mkdir -p "$HOME/.local/bin"
  printf "%s" "$HOME/.local/bin"
}

install_dir=$(choose_install_dir)
step "Installing to $install_dir"

if [ "$install_dir" = "/usr/local/bin" ] && [ ! -w /usr/local/bin ]; then
  sudo mv "$tmp/agentrun" "$install_dir/agentrun"
else
  mv "$tmp/agentrun" "$install_dir/agentrun"
fi
done_ "Binary installed at $install_dir/agentrun"

# ─── 5. PATH check ───────────────────────────────────────────────────────────
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *)
    warn "$install_dir is not on your PATH."
    warn "Add this to your shell rc:"
    echo "    export PATH=\"$install_dir:\$PATH\""
    ;;
esac

# ─── 6. Run agentrun install (unless skipped) ────────────────────────────────
if [ "${AGENTRUN_SKIP_HOOKS:-0}" = "1" ]; then
  warn "Skipping 'agentrun install' (AGENTRUN_SKIP_HOOKS=1)"
else
  step "Registering Claude + Codex hooks"
  "$install_dir/agentrun" install || warn "agentrun install reported a problem; check ~/.claude and ~/.codex"
fi

# ─── 7. OpenAI key hint ──────────────────────────────────────────────────────
if [ -z "${OPENAI_API_KEY:-}" ]; then
  warn "OPENAI_API_KEY is not set. Session summaries will be skipped."
  warn "To enable: export OPENAI_API_KEY=sk-... (add to your shell rc to persist)"
fi

done_ "All set. Try:"
echo "    agentrun doctor"
echo "    agentrun sessions"
echo "    claude   # this session will be recorded"
