#!/bin/sh
set -eu

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "${script_dir}/.." && pwd)

local_bin="${repo_root}/cc-connect"

can_run() {
  bin="$1"
  if "${bin}" --version >/dev/null 2>&1; then
    return 0
  fi
  if "${bin}" --help >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

path_bin=""
if command -v cc-connect >/dev/null 2>&1; then
  path_bin=$(command -v cc-connect 2>/dev/null || true)
fi

printf '%s\n' "cc-connect check"
printf '%s\n' "repo: ${repo_root}"

ok_any=0

if [ -x "${local_bin}" ]; then
  if can_run "${local_bin}"; then
    printf '%s\n' "ok: repo binary runnable: ${local_bin}"
    ok_any=1
  else
    printf '%s\n' "error: repo binary exists but cannot run --version/--help: ${local_bin}" >&2
  fi
else
  printf '%s\n' "note: repo binary not found (expected: ${local_bin})" >&2
fi

if [ -n "${path_bin}" ]; then
  if can_run "${path_bin}"; then
    printf '%s\n' "ok: PATH binary runnable: ${path_bin}"
    ok_any=1
  else
    printf '%s\n' "error: PATH binary exists but cannot run --version/--help: ${path_bin}" >&2
  fi
else
  printf '%s\n' "note: 'cc-connect' not found on PATH" >&2
fi

printf '\n'
printf '%s\n' "tooling:"
if command -v npm >/dev/null 2>&1; then
  printf '%s\n' "  ok: npm: $(command -v npm)"
else
  printf '%s\n' "  note: npm not found (npm global install path will be unavailable)" >&2
fi
if command -v go >/dev/null 2>&1; then
  printf '%s\n' "  ok: go:  $(command -v go)"
else
  printf '%s\n' "  note: go not found (source build path will be unavailable)" >&2
fi
if command -v make >/dev/null 2>&1; then
  printf '%s\n' "  ok: make: $(command -v make)"
else
  printf '%s\n' "  note: make not found (will skip Makefile-based builds)" >&2
fi

printf '\n'
printf '%s\n' "config hint:"
printf '%s\n' "  default: ./config.toml or ~/.cc-connect/config.toml"

if [ "${ok_any}" -eq 1 ]; then
  exit 0
fi

printf '\n' >&2
printf '%s\n' "error: no usable cc-connect executable found." >&2
printf '%s\n' "next:" >&2
printf '%s\n' "  ${repo_root}/scripts/install.sh" >&2
exit 1
