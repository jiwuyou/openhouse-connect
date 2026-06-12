#!/bin/sh
set -eu

usage() {
  cat <<'EOF'
Usage:
  scripts/install.sh

Behavior (idempotent):
  CC_CONNECT_INSTALL_MODE=auto (default):
    1) Prefer repo binary: ./cc-connect (if present and runnable)
    2) Else, prefer runnable cc-connect already on PATH
    3) Else, if npm is available: install cc-connect@beta globally
    4) Else, if go is available: build from source

  CC_CONNECT_INSTALL_MODE=local:
    Only accept ./cc-connect or a runnable cc-connect already on PATH.
    Never runs npm install or go build.

  CC_CONNECT_INSTALL_MODE=npm:
    Install cc-connect globally with npm.

  CC_CONNECT_INSTALL_MODE=source:
    Build with "make build-noweb" or a direct "go build" fallback.

Environment:
  CC_CONNECT_INSTALL_MODE  auto, local, npm, or source (default: auto)
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
mode="${CC_CONNECT_INSTALL_MODE:-auto}"

try_existing_binary() {
  if [ -x "${repo_bin}" ]; then
    if "${repo_bin}" --version >/dev/null 2>&1 || "${repo_bin}" --help >/dev/null 2>&1; then
      log "ok: using existing repo binary: ${repo_bin}"
      return 0
    fi
    warn "note: repo binary exists but did not run: ${repo_bin}"
  fi

  path_bin=""
  if command -v cc-connect >/dev/null 2>&1; then
    path_bin=$(command -v cc-connect 2>/dev/null || true)
    if [ -n "${path_bin}" ]; then
      if cc-connect --version >/dev/null 2>&1 || cc-connect --help >/dev/null 2>&1; then
        log "ok: using existing PATH binary: ${path_bin}"
        return 0
      fi
      warn "note: 'cc-connect' found on PATH but did not run; continuing."
    fi
  fi

  return 1
}

install_from_npm() {
  npm_pkg=${CC_CONNECT_NPM_PACKAGE:-cc-connect@beta}

  if ! command -v npm >/dev/null 2>&1; then
    warn "error: npm not found; npm install mode requires Node.js/npm."
    return 1
  fi

  log "install: npm global install: ${npm_pkg}"
  if npm install -g "${npm_pkg}"; then
    if command -v cc-connect >/dev/null 2>&1 && cc-connect --version >/dev/null 2>&1; then
      log "ok: installed cc-connect to PATH: $(command -v cc-connect)"
      return 0
    fi
    warn "note: npm install succeeded, but 'cc-connect' is still not runnable via PATH."
    warn "note: check your npm prefix/bin directory and PATH."
    if command -v npm >/dev/null 2>&1; then
      warn "  npm prefix -g: $(npm prefix -g 2>/dev/null || printf '?')"
    fi
    return 1
  fi
  warn "error: npm install failed (permissions/network?)."
  warn "note: override npm package via CC_CONNECT_NPM_PACKAGE=..."
  return 1
}

build_from_source() {
  if ! command -v go >/dev/null 2>&1; then
    warn "error: go not found; source install mode requires a Go toolchain."
    return 1
  fi

  log "install: building from source (no web assets)"
  if ! (
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
  ); then
    warn "error: source build failed."
    return 1
  fi

  if [ -x "${repo_bin}" ] && "${repo_bin}" --version >/dev/null 2>&1; then
    log "ok: built repo binary: ${repo_bin}"
    return 0
  fi

  warn "error: build completed but repo binary is missing or not runnable: ${repo_bin}"
  return 1
}

case "${mode}" in
  auto)
    if try_existing_binary; then
      exit 0
    fi
    if command -v npm >/dev/null 2>&1; then
      install_from_npm
      exit $?
    fi
    if command -v go >/dev/null 2>&1; then
      build_from_source
      exit $?
    fi
    warn "error: cannot install cc-connect (need one of: existing ./cc-connect, npm, or go)."
    warn "note: install Node.js/npm for prebuilt binaries, or Go toolchain for source builds."
    exit 1
    ;;
  local)
    if try_existing_binary; then
      exit 0
    fi
    warn "error: offline local install payload is missing a runnable cc-connect binary."
    warn "note: expected ${repo_bin} or cc-connect already on PATH."
    warn "note: CC_CONNECT_INSTALL_MODE=local never runs npm install or go build."
    exit 1
    ;;
  npm)
    install_from_npm
    exit $?
    ;;
  source)
    build_from_source
    exit $?
    ;;
  *)
    warn "error: unknown CC_CONNECT_INSTALL_MODE: ${mode}"
    warn "note: expected one of: auto, local, npm, source."
    exit 1
    ;;
esac
