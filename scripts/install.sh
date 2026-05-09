#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
Usage:
  scripts/install.sh

Behavior (idempotent):
  1) Prefer repo binary: ./cc-connect (if present and runnable)
  2) Else, if npm is available: install cc-connect@beta globally (override with CC_CONNECT_NPM_PACKAGE)
  3) Else, if go is available: build with "make build-noweb" or a direct "go build" fallback

Environment:
  CC_CONNECT_NPM_PACKAGE   npm package spec to install (default: cc-connect@beta)
EOF
}

case "${1:-}" in
  -h|--help)
    usage
    exit 0
    ;;
esac

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "${script_dir}/.." && pwd)

log() { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }

repo_bin="${repo_root}/cc-connect"

if [ -x "${repo_bin}" ]; then
  if "${repo_bin}" --version >/dev/null 2>&1 || "${repo_bin}" --help >/dev/null 2>&1; then
    log "ok: using existing repo binary: ${repo_bin}"
    exit 0
  fi
  warn "note: repo binary exists but did not run: ${repo_bin}"
fi

path_bin=""
if command -v cc-connect >/dev/null 2>&1; then
  path_bin=$(command -v cc-connect 2>/dev/null || true)
  if [ -n "${path_bin}" ]; then
    if cc-connect --version >/dev/null 2>&1 || cc-connect --help >/dev/null 2>&1; then
      log "ok: using existing PATH binary: ${path_bin}"
      exit 0
    fi
    warn "note: 'cc-connect' found on PATH but did not run; continuing."
  fi
fi

npm_pkg=${CC_CONNECT_NPM_PACKAGE:-cc-connect@beta}

if command -v npm >/dev/null 2>&1; then
  log "install: npm global install: ${npm_pkg}"
  if npm install -g "${npm_pkg}"; then
    if command -v cc-connect >/dev/null 2>&1 && cc-connect --version >/dev/null 2>&1; then
      log "ok: installed cc-connect to PATH: $(command -v cc-connect)"
      exit 0
    fi
    warn "note: npm install succeeded, but 'cc-connect' is still not runnable via PATH."
    warn "note: check your npm prefix/bin directory and PATH."
    if command -v npm >/dev/null 2>&1; then
      warn "  npm prefix -g: $(npm prefix -g 2>/dev/null || printf '?')"
    fi
    exit 1
  fi
  warn "error: npm install failed (permissions/network?)."
  warn "note: override npm package via CC_CONNECT_NPM_PACKAGE=..."
  exit 1
fi

if command -v go >/dev/null 2>&1; then
  log "install: building from source (no web assets)"
  (
    cd "${repo_root}"
    if command -v make >/dev/null 2>&1; then
      if make build-noweb; then
        :
      else
        warn "note: 'make build-noweb' failed; falling back to 'go build'."
        go build -tags 'no_web' -o cc-connect ./cmd/cc-connect
      fi
    else
      go build -tags 'no_web' -o cc-connect ./cmd/cc-connect
    fi
  )

  if [ -x "${repo_bin}" ] && "${repo_bin}" --version >/dev/null 2>&1; then
    log "ok: built repo binary: ${repo_bin}"
    exit 0
  fi

  warn "error: build completed but repo binary is missing or not runnable: ${repo_bin}"
  exit 1
fi

warn "error: cannot install cc-connect (need one of: existing ./cc-connect, npm, or go)."
warn "note: install Node.js/npm for prebuilt binaries, or Go toolchain for source builds."
exit 1
