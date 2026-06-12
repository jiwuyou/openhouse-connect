#!/bin/sh
set -eu

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "${script_dir}/.." && pwd)

log() { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }

svc_name="cc-connect"
svc_desc="OpenHouse Connect (cc-connect): agent bridge + webclient"

# OpenHouse SmallPhoneAI ports.
bridge_port=${CC_CONNECT_BRIDGE_PORT:-21010}
management_port=${CC_CONNECT_MANAGEMENT_PORT:-21020}
webclient_port=${CC_CONNECT_WEBCLIENT_PORT:-21040}
bridge_token=${CC_CONNECT_BRIDGE_TOKEN:-smallphoneai-bridge-token}
management_token=${CC_CONNECT_MANAGEMENT_TOKEN:-smallphoneai-management-token}
webclient_token=${CC_CONNECT_WEBCLIENT_TOKEN:-smallphoneai-webclient-token}
smallphone_work_dir=${CC_CONNECT_SMALLPHONE_WORK_DIR:-/root/smallphoneai-repos/smallphone-active}
smallphone_agent_type=${CC_CONNECT_SMALLPHONE_AGENT_TYPE:-claudecode}
smallphone_agent_mode=${CC_CONNECT_SMALLPHONE_AGENT_MODE:-default}
smallphone_claude_cli=${CC_CONNECT_SMALLPHONE_CLAUDE_CLI:-/root/.npm-global/bin/claude}
service_path="/root/.npm-global/bin:/root/.local/node/bin:/root/.opencode/bin:/root/.local/bin:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin:/system/bin:/system/xbin:/data/data/com.termux/files/usr/bin"

tag_group="group:local-stack"
tag_component="openhouse-component:cc-connect"

bin_path=""
if [ -x "${repo_root}/cc-connect" ]; then
  bin_path="${repo_root}/cc-connect"
elif command -v cc-connect >/dev/null 2>&1; then
  bin_path=$(command -v cc-connect 2>/dev/null || true)
fi

log "cc-connect service registration (best-effort)"
log "service:"
log "  name: ${svc_name}"
log "  description: ${svc_desc}"
log "  tags: ${tag_group}, ${tag_component}"
log "ports:"
log "  ${bridge_port}  (bridge/ws)"
log "  ${management_port}  (management api/ui)"
log "  ${webclient_port}  (webclient facade)"

if [ -z "${bin_path}" ]; then
  warn
  warn "note: no cc-connect binary found yet."
  warn "  expected repo binary: ${repo_root}/cc-connect"
  warn "  or PATH: cc-connect"
  warn "next:"
  warn "  ${repo_root}/scripts/install.sh"
  exit 0
fi

cfg_path="${CC_CONNECT_CONFIG_PATH:-${repo_root}/config.smallphoneai.toml}"

ensure_smallphoneai_config() {
  if [ -f "${cfg_path}" ]; then
    if grep -Fq '[[projects]]' "${cfg_path}" \
      && grep -Fq 'type = "claudecode"' "${cfg_path}" \
      && grep -Fq 'cli_path = "' "${cfg_path}" \
      && ! grep -Fq '${CC_CONNECT_' "${cfg_path}"; then
      return 0
    fi
    warn "note: refreshing stale SmallPhoneAI cc-connect config: ${cfg_path}"
  fi

  mkdir -p "$(dirname "${cfg_path}")"
  cat >"${cfg_path}" <<EOF
[log]
level = "info"

[bridge]
enabled = true
host = "127.0.0.1"
port = ${bridge_port}
token = "${bridge_token}"
path = "/bridge/ws"
cors_origins = ["*"]

[management]
enabled = true
host = "127.0.0.1"
port = ${management_port}
token = "${management_token}"
cors_origins = ["http://127.0.0.1:${webclient_port}", "http://localhost:${webclient_port}"]

[webclient]
enabled = true
host = "127.0.0.1"
port = ${webclient_port}
token = "${webclient_token}"
public_url = "http://127.0.0.1:${webclient_port}"
default_app = "smallphone"

[[webclient.apps]]
id = "smallphone"
platform = "webclient-smallphone"
data_namespace = "smallphone"
enabled = true

[[projects]]
name = "smallphone"
display_name = "SmallPhone"

[projects.agent]
type = "${smallphone_agent_type}"

[projects.agent.options]
work_dir = "${smallphone_work_dir}"
cli_path = "${smallphone_claude_cli}"
mode = "${smallphone_agent_mode}"
EOF
}

ensure_smallphoneai_config

cfg_arg=""
if [ -n "${cfg_path}" ]; then
  cfg_arg="--config ${cfg_path}"
fi

log
log "exec:"
if [ -n "${cfg_arg}" ]; then
  log "  ${bin_path} ${cfg_arg}"
else
  log "  ${bin_path}"
fi

# service-manager registration (optional; guarded).
sm_url="${SERVICE_MANAGER_URL:-http://127.0.0.1:20087}"

if ! command -v service-manager >/dev/null 2>&1; then
  warn
  warn "note: service-manager CLI not found on PATH; skipping registration."
  warn "  expected: service-manager serve --bind 127.0.0.1:20087"
  exit 0
fi

if ! command -v curl >/dev/null 2>&1; then
  warn
  warn "note: curl not found; skipping registration."
  exit 0
fi

if ! curl -fsS --max-time 2 "${sm_url}/api/v1/health" >/dev/null 2>&1; then
  warn
  warn "note: service-manager does not appear reachable at: ${sm_url}"
  warn "  start it with: service-manager serve --bind 127.0.0.1:20087"
  warn "  or set SERVICE_MANAGER_URL=http://host:port"
  exit 0
fi

sm_token="${SERVICE_MANAGER_TOKEN:-}"
if [ -z "${sm_token}" ]; then
  # Capture token without printing it.
  sm_token=$(service-manager token show 2>/dev/null | tr -d '\r\n' || true)
fi

if [ -z "${sm_token}" ]; then
  warn
  warn "note: could not obtain service-manager token; skipping registration."
  warn "  set SERVICE_MANAGER_TOKEN=... or run: service-manager token show"
  exit 0
fi

umask 077
tmp_base="${TMPDIR:-/tmp}"
work_dir=""
if command -v mktemp >/dev/null 2>&1; then
  work_dir=$(mktemp -d "${tmp_base%/}/cc-connect-sm.XXXXXX" 2>/dev/null || true)
fi
if [ -z "${work_dir}" ] || [ ! -d "${work_dir}" ]; then
  warn
  warn "note: mktemp not available; skipping service-manager registration to avoid predictable temp files."
  exit 0
fi

curl_cfg="${work_dir}/curl.cfg"
spec_file="${work_dir}/service-spec.json"
cleanup() {
  if [ -n "${work_dir}" ] && [ -d "${work_dir}" ]; then
    rm -f "${curl_cfg}" "${spec_file}" >/dev/null 2>&1 || true
    rmdir "${work_dir}" >/dev/null 2>&1 || true
  fi
}
trap cleanup 0 INT HUP TERM

# Use a curl config file so bearer tokens do not end up in process listings.
printf 'header = "Authorization: Bearer %s"\n' "${sm_token}" >"${curl_cfg}"
printf 'header = "Content-Type: application/json"\n' >>"${curl_cfg}"

# Build ServiceSpec JSON.
# Note: we avoid embedding secrets; cc-connect reads its own config.toml/token values.
py=""
if command -v python3 >/dev/null 2>&1; then
  py="python3"
elif command -v python >/dev/null 2>&1; then
  py="python"
fi

if [ -z "${py}" ]; then
  warn
  warn "note: python not found; skipping idempotent service-manager upsert."
  warn "  (would need JSON tooling to find existing service by name)"
  exit 0
fi

${py} - "${svc_name}" "${svc_desc}" "${bin_path}" "${repo_root}" "${cfg_path}" \
  "${bridge_port}" "${management_port}" "${webclient_port}" \
  "${tag_group}" "${tag_component}" "${service_path}" >"${spec_file}" <<'PY'
import json
import sys

(
    svc_name,
    svc_desc,
    bin_path,
    repo_root,
    cfg_path,
    bridge_port,
    mgmt_port,
    webclient_port,
    tag_group,
    tag_component,
    service_path,
) = sys.argv[1:]

cmd = [bin_path]
if cfg_path:
    cmd += ["--config", cfg_path]

def tcp_check(port: str):
    return {
        "type": "tcp",
        "address": f"127.0.0.1:{port}",
        "interval": "30s",
        "timeout": "3s",
    }

spec = {
    "name": svc_name,
    "description": svc_desc,
    "provider": "process",
    "command": cmd,
    "working_dir": repo_root,
    "env": {"PATH": service_path},
    "runtime": {},
    "restart": {"mode": "always", "max_retries": 0},
    "health": [tcp_check(bridge_port), tcp_check(mgmt_port), tcp_check(webclient_port)],
    "enabled": True,
    "tags": [tag_group, tag_component],
}

json.dump(spec, sys.stdout, ensure_ascii=True)
PY

services_json=""
if ! services_json=$(curl -q -fsS --max-time 3 -K "${curl_cfg}" "${sm_url}/api/v1/services" 2>/dev/null); then
  warn
  warn "note: failed to list services from service-manager (auth token / server error)."
  warn "note: skipping registration; use the service-manager Web UI to add/update the service."
  exit 0
fi
svc_id=$(
  printf '%s' "${services_json}" | ${py} -c '
import json, sys

name = sys.argv[1]
data = sys.stdin.read().strip()
if not data:
    sys.exit(0)

try:
    services = json.loads(data)
except Exception:
    sys.exit(0)

if not isinstance(services, list):
    sys.exit(0)

for svc in services:
    if not isinstance(svc, dict):
        continue
    spec = svc.get("spec", {})
    if isinstance(spec, dict) and spec.get("name") == name:
        sid = svc.get("id", "")
        if isinstance(sid, str) and sid:
            sys.stdout.write(sid)
            break
' "${svc_name}" 2>/dev/null || true
)

if [ -n "${svc_id}" ]; then
  log
  log "service-manager: updating existing service (name=${svc_name})"
  if curl -q -fsS --max-time 5 -X PUT -K "${curl_cfg}" --data-binary "@${spec_file}" "${sm_url}/api/v1/services/${svc_id}" >/dev/null 2>&1; then
    curl -q -fsS --max-time 5 -X POST -K "${curl_cfg}" "${sm_url}/api/v1/services/${svc_id}/register" >/dev/null 2>&1 || true
    log "service-manager: updated and registered: ${svc_name}"
  else
    warn "note: failed to update service; leaving existing record unchanged."
  fi
  exit 0
fi

log
log "service-manager: creating new service (name=${svc_name})"
create_resp=""
if ! create_resp=$(curl -q -fsS --max-time 5 -X POST -K "${curl_cfg}" --data-binary "@${spec_file}" "${sm_url}/api/v1/services" 2>/dev/null); then
  warn "note: failed to create service; check service-manager logs and token."
  exit 0
fi
created_id=$(
  printf '%s' "${create_resp}" | ${py} -c '
import json, sys
data = sys.stdin.read().strip()
if not data:
    sys.exit(0)
try:
    svc = json.loads(data)
except Exception:
    sys.exit(0)
if isinstance(svc, dict):
    sid = svc.get("id", "")
    if isinstance(sid, str) and sid:
        sys.stdout.write(sid)
' 2>/dev/null || true
)
if [ -n "${created_id}" ]; then
  curl -q -fsS --max-time 5 -X POST -K "${curl_cfg}" "${sm_url}/api/v1/services/${created_id}/register" >/dev/null 2>&1 || true
  log "service-manager: created and registered: ${svc_name}"
else
  warn "note: service created, but could not parse id for follow-up registration."
  warn "note: use the service-manager Web UI to confirm registration/tags."
fi
exit 0
