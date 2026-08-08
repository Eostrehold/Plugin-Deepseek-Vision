#!/usr/bin/env bash
# Opt-in, billable E2E through real DeepSeek and vision providers.
#
# The deterministic phase sends a Codex-shaped two-request tool loop through a
# real CLIProxyAPI process. LIVE_USE_CODEX=1 adds a genuine `codex exec` loop.
# This script is intentionally excluded from ordinary CI.
set -euo pipefail
umask 077

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CLI_ROOT=${CLIPROXY_ROOT:-}
TIMEOUT=${LIVE_TIMEOUT_SECONDS:-300}
DEEPSEEK_BASE_URL=${DEEPSEEK_BASE_URL:-https://api.deepseek.com/v1}
DEEPSEEK_MODEL=${DEEPSEEK_MODEL:-deepseek-v4-flash}
GPT_BASE_URL=${GPT_BASE_URL:-}
VISION_MODEL=${VISION_MODEL:-glm-4.6v-flash}
VISION_FALLBACK_MODELS=${VISION_FALLBACK_MODELS:-}
LIVE_USE_CODEX=${LIVE_USE_CODEX:-0}
LIVE_EXPECT_FALLBACK=${LIVE_EXPECT_FALLBACK:-0}
LIVE_FORCE_PRIMARY_STATUS=${LIVE_FORCE_PRIMARY_STATUS:-}
KEEP_LIVE_TMP=${KEEP_LIVE_TMP:-0}

require_value() {
  local name=$1
  [[ -n ${!name:-} ]] || { echo "set $name before running live-provider-e2e" >&2; exit 2; }
}

for command_name in curl python3 go; do
  command -v "$command_name" >/dev/null 2>&1 || { echo "live-provider-e2e requires $command_name" >&2; exit 2; }
done
require_value DEEPSEEK_API_KEY
require_value GPT_API_KEY
require_value GPT_BASE_URL
if [[ -z "$CLI_ROOT" || ! -d "$CLI_ROOT" ]]; then
  echo "set CLIPROXY_ROOT to a CLIProxyAPI checkout compatible with v7.2.121" >&2
  exit 2
fi
if [[ "$LIVE_USE_CODEX" != "0" && "$LIVE_USE_CODEX" != "1" ]]; then
  echo "LIVE_USE_CODEX must be 0 or 1" >&2
  exit 2
fi
if [[ "$LIVE_EXPECT_FALLBACK" != "0" && "$LIVE_EXPECT_FALLBACK" != "1" ]]; then
  echo "LIVE_EXPECT_FALLBACK must be 0 or 1" >&2
  exit 2
fi
if [[ "$KEEP_LIVE_TMP" != "0" && "$KEEP_LIVE_TMP" != "1" ]]; then
  echo "KEEP_LIVE_TMP must be 0 or 1" >&2
  exit 2
fi
if [[ "$LIVE_EXPECT_FALLBACK" == "1" && -z "$VISION_FALLBACK_MODELS" ]]; then
  echo "LIVE_EXPECT_FALLBACK=1 requires at least one VISION_FALLBACK_MODELS entry" >&2
  exit 2
fi
if [[ -n "$LIVE_FORCE_PRIMARY_STATUS" ]]; then
  if [[ ! "$LIVE_FORCE_PRIMARY_STATUS" =~ ^[0-9]{3}$ ]]; then
    echo "LIVE_FORCE_PRIMARY_STATUS must be 408, 429, or a 5xx status" >&2
    exit 2
  fi
  force_status_decimal=$((10#$LIVE_FORCE_PRIMARY_STATUS))
  if (( force_status_decimal != 408 && force_status_decimal != 429 && (force_status_decimal < 500 || force_status_decimal > 599) )); then
    echo "LIVE_FORCE_PRIMARY_STATUS must be 408, 429, or a 5xx status" >&2
    exit 2
  fi
  if [[ "$LIVE_EXPECT_FALLBACK" != "1" || -z "$VISION_FALLBACK_MODELS" ]]; then
    echo "LIVE_FORCE_PRIMARY_STATUS requires LIVE_EXPECT_FALLBACK=1 and VISION_FALLBACK_MODELS" >&2
    exit 2
  fi
fi
if [[ "$LIVE_USE_CODEX" == "1" ]] && ! command -v codex >/dev/null 2>&1; then
  echo "LIVE_USE_CODEX=1 requires codex on PATH" >&2
  exit 2
fi
if [[ "$LIVE_USE_CODEX" == "1" ]] && ! command -v timeout >/dev/null 2>&1; then
  echo "LIVE_USE_CODEX=1 requires timeout on PATH" >&2
  exit 2
fi

normalize_openai_base() {
  python3 - "$1" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit

value = sys.argv[1].strip().rstrip("/")
parts = urlsplit(value)
if parts.scheme not in {"http", "https"} or not parts.netloc:
    raise SystemExit("provider base URL must be an absolute http(s) URL")
path = parts.path.rstrip("/")
if not path:
    path = "/v1"
print(urlunsplit((parts.scheme, parts.netloc, path, parts.query, parts.fragment)))
PY
}

DEEPSEEK_BASE_URL=$(normalize_openai_base "$DEEPSEEK_BASE_URL")
GPT_BASE_URL=$(normalize_openai_base "$GPT_BASE_URL")
export DEEPSEEK_BASE_URL DEEPSEEK_MODEL GPT_BASE_URL VISION_MODEL VISION_FALLBACK_MODELS
export LIVE_EXPECT_FALLBACK LIVE_FORCE_PRIMARY_STATUS

TMP=$(mktemp -d "${TMPDIR:-/tmp}/deepseek-vision-live-e2e.XXXXXX")
PLUGIN_DIR="$TMP/plugins"
CONFIG="$TMP/config.yaml"
HOST_PID=""
FAULT_PID=""
mkdir -p "$PLUGIN_DIR/linux/amd64" "$TMP/auth" "$TMP/codex-work"

redact_text_files() {
  python3 - "$TMP" <<'PY'
import os
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
secrets = [os.environ.get("DEEPSEEK_API_KEY", ""), os.environ.get("GPT_API_KEY", "")]
suffixes = {".json", ".jsonl", ".log", ".out", ".txt", ".toml", ".yaml"}
for path in root.rglob("*"):
    if not path.is_file() or path.suffix not in suffixes:
        continue
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        continue
    changed = text
    for secret in secrets:
        if secret:
            changed = changed.replace(secret, "[REDACTED]")
    if path.name.endswith(".log") or path.name.endswith(".out"):
        changed = re.sub(r"data:image/[^;,\s]+;base64,[A-Za-z0-9+/=]+", "[REDACTED_DATA_URI]", changed)
    if changed != text:
        path.write_text(changed, encoding="utf-8")
PY
}

verify_no_credentials() {
  python3 - "$TMP" <<'PY'
import os
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
secrets = [value.encode() for value in (os.environ.get("DEEPSEEK_API_KEY", ""), os.environ.get("GPT_API_KEY", "")) if value]
for path in root.rglob("*"):
    if not path.is_file():
        continue
    raw = path.read_bytes()
    if any(secret in raw for secret in secrets):
        raise SystemExit("credential material remains in preserved artifacts")
PY
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [[ -n "$HOST_PID" ]] && kill -0 "$HOST_PID" 2>/dev/null; then
    kill "$HOST_PID" 2>/dev/null || true
  fi
  if [[ -n "$HOST_PID" ]]; then
    wait "$HOST_PID" 2>/dev/null || true
  fi
  if [[ -n "$FAULT_PID" ]] && kill -0 "$FAULT_PID" 2>/dev/null; then
    kill "$FAULT_PID" 2>/dev/null || true
  fi
  if [[ -n "$FAULT_PID" ]]; then
    wait "$FAULT_PID" 2>/dev/null || true
  fi
  if ! rm -f "$CONFIG"; then
    echo "live-provider-e2e could not remove the credential-bearing config; deleting temporary directory" >&2
    rm -rf "$TMP"
    exit 1
  fi
  if [[ "$KEEP_LIVE_TMP" == "1" ]]; then
    if redact_text_files && verify_no_credentials; then
      echo "live-provider-e2e sanitized artifacts preserved at: $TMP" >&2
    else
      echo "live-provider-e2e could not prove artifact sanitization; deleting temporary directory" >&2
      rm -rf "$TMP"
      rc=1
    fi
  else
    rm -rf "$TMP"
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

safe_log_excerpt() {
  local path=$1
  python3 - "$path" <<'PY'
import os
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
text = path.read_text(encoding="utf-8", errors="replace") if path.exists() else ""
for name in ("DEEPSEEK_API_KEY", "GPT_API_KEY"):
    secret = os.environ.get(name, "")
    if secret:
        text = text.replace(secret, "[REDACTED]")
text = re.sub(r"data:image/[^;,\s]+;base64,[A-Za-z0-9+/=]+", "[REDACTED_DATA_URI]", text)
print(text[-4000:])
PY
}

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

HOST_PORT=$(free_port)

if [[ -n "$LIVE_FORCE_PRIMARY_STATUS" ]]; then
  FAULT_PORT=$(free_port)
  LIVE_FAULT_MODEL="live-forced-$LIVE_FORCE_PRIMARY_STATUS"
  LIVE_FAULT_BASE_URL="http://127.0.0.1:$FAULT_PORT/v1"
  export LIVE_FAULT_MODEL LIVE_FAULT_BASE_URL
  python3 - "$FAULT_PORT" "$LIVE_FORCE_PRIMARY_STATUS" >"$TMP/fault.log" 2>&1 <<'PY' &
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port, status = map(int, sys.argv[1:])

class Handler(BaseHTTPRequestHandler):
    def log_message(self, _format, *_args):
        return

    def reply(self, code, payload):
        raw = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path.endswith("/healthz"):
            self.reply(200, {"ok": True})
        else:
            self.reply(404, {"error": "not_found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        self.reply(status, {"error": {"type": "forced_retryable_status"}})

ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY
  FAULT_PID=$!
  deadline=$((SECONDS + 10))
  until curl -fsS --max-time 1 "http://127.0.0.1:$FAULT_PORT/healthz" >/dev/null 2>&1; do
    kill -0 "$FAULT_PID" 2>/dev/null || { echo "forced-status fixture exited during startup" >&2; exit 1; }
    (( SECONDS < deadline )) || { echo "forced-status fixture startup timed out" >&2; exit 1; }
    sleep 0.1
  done
fi

echo "building plugin and temporary CLIProxyAPI host"
CGO_ENABLED=1 GOTOOLCHAIN=auto go build -buildmode=c-shared -trimpath \
  -ldflags='-s -w -X main.pluginVersion=0.3.0-live-e2e' \
  -o "$PLUGIN_DIR/linux/amd64/deepseek-vision-v0.3.0.so" "$ROOT"
(cd "$CLI_ROOT" && CGO_ENABLED=1 GOTOOLCHAIN=auto go build -trimpath -o "$TMP/cliproxy" ./cmd/server)

python3 - "$CONFIG" "$PLUGIN_DIR" "$HOST_PORT" <<'PY'
import json
import os
import pathlib
import sys

path, plugin_dir, host_port = sys.argv[1:]
deepseek_model = os.environ.get("DEEPSEEK_MODEL", "deepseek-v4-flash").strip()
vision_model = os.environ.get("VISION_MODEL", "glm-4.6v-flash").strip()
fallbacks = [item.strip() for item in os.environ.get("VISION_FALLBACK_MODELS", "").split(",") if item.strip()]
fault_model = os.environ.get("LIVE_FAULT_MODEL", "").strip()
configured_primary = fault_model or vision_model
chain = [configured_primary, *fallbacks]
real_vision_models = fallbacks if fault_model else chain
if not deepseek_model or not vision_model:
    raise SystemExit("model names must not be empty")
if len(fallbacks) > 3 or len(set(chain)) != len(chain):
    raise SystemExit("vision fallback chain must contain at most three unique fallback models")
if deepseek_model in chain:
    raise SystemExit("the text target model must not also appear in the vision model chain")

config = {
    "host": "127.0.0.1",
    "port": int(host_port),
    "debug": False,
    "auth-dir": str(pathlib.Path(path).parent / "auth"),
    "api-keys": ["live-client-key"],
    "remote-management": {
        "allow-remote": False,
        "secret-key": "live-management-key",
        "disable-control-panel": True,
    },
    "plugins": {
        "enabled": True,
        "dir": "plugins",
        "configs": {
            "deepseek-vision": {
                "enabled": True,
                "priority": 100,
                "store": {"source": "live-provider-e2e", "version": "0.3.0"},
                "target_models": [deepseek_model],
                "vision_model": configured_primary,
                "vision_fallback_models": fallbacks,
                "agent_reanalysis_enabled": True,
                "language": "en",
                "request_timeout_seconds": 90,
                "max_inflight_vision_requests": 2,
                "emergency_max_images_per_request": 16,
                "max_request_bytes": 20 * 1024 * 1024,
                "max_image_reference_bytes": 4 * 1024 * 1024,
                "max_response_bytes": 4 * 1024 * 1024,
                "max_result_chars": 12000,
                "analysis_cache_size": 32,
                "analysis_cache_ttl_seconds": 60,
                "analysis_url_cache_ttl_seconds": 30,
                "trace_enabled": True,
            }
        },
    },
    "openai-compatibility": ([
        {
            "name": "live-forced-status-provider",
            "base-url": os.environ["LIVE_FAULT_BASE_URL"],
            "api-key-entries": [{"api-key": "forced-status-key"}],
            "models": [{"name": fault_model, "alias": fault_model, "input-modalities": ["text", "image"]}],
        }
    ] if fault_model else []) + [
        {
            "name": "live-vision-provider",
            "base-url": os.environ["GPT_BASE_URL"],
            "api-key-entries": [{"api-key": os.environ["GPT_API_KEY"]}],
            "models": [
                {"name": model, "alias": model, "input-modalities": ["text", "image"]}
                for model in real_vision_models
            ],
        },
        {
            "name": "live-deepseek-provider",
            "base-url": os.environ["DEEPSEEK_BASE_URL"],
            "api-key-entries": [{"api-key": os.environ["DEEPSEEK_API_KEY"]}],
            "models": [
                {"name": deepseek_model, "alias": deepseek_model, "input-modalities": ["text", "image"]}
            ],
        },
    ],
}
pathlib.Path(path).write_text(json.dumps(config, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
PY
chmod 600 "$CONFIG"

(cd "$TMP" && env -u DEEPSEEK_API_KEY -u GPT_API_KEY "$TMP/cliproxy" -config "$CONFIG" -local-model >"$TMP/host.log" 2>&1) &
HOST_PID=$!
BASE="http://127.0.0.1:$HOST_PORT"

deadline=$((SECONDS + TIMEOUT))
until curl -fsS --max-time 2 -H 'Authorization: Bearer live-client-key' "$BASE/v1/models" >/dev/null 2>&1; do
  if ! kill -0 "$HOST_PID" 2>/dev/null; then
    echo "CLIProxyAPI exited during live-provider startup" >&2
    safe_log_excerpt "$TMP/host.log" >&2
    exit 1
  fi
  (( SECONDS < deadline )) || { echo "CLIProxyAPI live-provider startup timed out" >&2; exit 1; }
  sleep 0.25
done

CODEX_HOME="$TMP/.codex"
ATTACHMENT_DIR="$CODEX_HOME/attachments/att_live"
IMAGE_PATH="$ATTACHMENT_DIR/four-bands.png"
mkdir -p "$ATTACHMENT_DIR"

python3 - "$IMAGE_PATH" "$TMP/initial-request.json" "$TMP/reanalysis-request.json" <<'PY'
import base64
import json
import os
import pathlib
import struct
import sys
import zlib

image_path, initial_path, reanalysis_path = map(pathlib.Path, sys.argv[1:])
width, height = 128, 72
colors = [(220, 40, 40), (245, 205, 45), (45, 180, 75), (45, 90, 210)]
rows = []
for _y in range(height):
    row = bytearray([0])
    for x in range(width):
        row.extend(colors[min(x // 32, 3)])
    rows.append(bytes(row))

def chunk(kind: bytes, data: bytes) -> bytes:
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)

png = b"\x89PNG\r\n\x1a\n"
png += chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
png += chunk(b"IDAT", zlib.compress(b"".join(rows), 9))
png += chunk(b"IEND", b"")
image_path.write_bytes(png)
data_uri = "data:image/png;base64," + base64.b64encode(png).decode("ascii")
model = os.environ.get("DEEPSEEK_MODEL", "deepseek-v4-flash")

tools = [
    {
        "type": "function",
        "name": "view_image",
        "description": "Inspect an attached image at the supplied path.",
        "parameters": {
            "type": "object",
            "properties": {"path": {"type": "string"}, "detail": {"type": "string", "enum": ["high", "original"]}},
            "required": ["path"],
            "additionalProperties": False,
        },
    },
    {
        "type": "function",
        "name": "deepseek_vision_reanalyze",
        "description": "Request a focused reanalysis of Agent-owned image attachments.",
        "parameters": {
            "type": "object",
            "properties": {
                "attachment_ids": {"type": "array", "items": {"type": "string"}},
                "focus": {"type": "string"},
                "detail": {"type": "string", "enum": ["high", "original"]},
                "cache": {"type": "string", "enum": ["refresh", "no_store"]},
            },
            "required": ["attachment_ids", "focus"],
            "additionalProperties": False,
        },
    },
]

initial = {
    "model": model,
    "tools": tools,
    "input": [{"role": "user", "content": [
        {"type": "input_text", "text": "Give a short general description of the attached synthetic image."},
        {"type": "input_image", "image_url": data_uri},
    ]}],
    "stream": False,
}
focus = "Inspect only the number of vertical bands and their left-to-right order."
reanalysis = {
    "model": model,
    "tools": tools,
    "input": [
        {"role": "user", "content": [{"type": "input_text", "text": focus}]},
        {"type": "function_call", "name": "view_image", "call_id": "live_view_001", "arguments": json.dumps({"path": str(image_path), "detail": "original"})},
        {"type": "function_call", "name": "deepseek_vision_reanalyze", "call_id": "live_reanalyze_001", "arguments": json.dumps({
            "attachment_ids": ["att_live"],
            "focus": "Report only the four band colors from left to right.",
            "detail": "original",
            "cache": "no_store",
        })},
        {"type": "function_call_output", "call_id": "live_view_001", "output": [{"type": "input_image", "image_url": data_uri}]},
        {"type": "function_call_output", "call_id": "live_reanalyze_001", "output": [{"type": "input_image", "image_url": data_uri}]},
    ],
    "stream": False,
}
initial_path.write_text(json.dumps(initial, separators=(",", ":")), encoding="utf-8")
reanalysis_path.write_text(json.dumps(reanalysis, separators=(",", ":")), encoding="utf-8")
PY
chmod 600 "$IMAGE_PATH" "$TMP/initial-request.json" "$TMP/reanalysis-request.json"

post_response() {
  local request_file=$1 response_file=$2
  curl -sS --max-time "$TIMEOUT" -o "$response_file" -w '%{http_code}' \
    -H 'Authorization: Bearer live-client-key' -H 'Content-Type: application/json' \
    -X POST "$BASE/v1/responses" --data-binary "@$request_file"
}

assert_live_response() {
  local label=$1 request_file=$2 response_file="$TMP/$1.out" code
  code=$(post_response "$request_file" "$response_file")
  if [[ "$code" != "200" ]]; then
    echo "$label failed with HTTP $code" >&2
    safe_log_excerpt "$response_file" >&2
    exit 1
  fi
  python3 - "$response_file" <<'PY'
import json
import sys

payload = json.load(open(sys.argv[1], encoding="utf-8"))
assert payload.get("error") is None, payload
assert payload.get("id") or payload.get("output") or payload.get("output_text"), payload
PY
}

echo "running billable deterministic live-provider requests"
assert_live_response initial "$TMP/initial-request.json"
python3 - "$TMP/initial.out" "$TMP/reanalysis-request.json" <<'PY'
import json
import pathlib
import sys

response_path, request_path = map(pathlib.Path, sys.argv[1:])
response = json.loads(response_path.read_text(encoding="utf-8"))
request = json.loads(request_path.read_text(encoding="utf-8"))
# Thinking-mode Responses providers require their opaque reasoning item to be
# echoed before the assistant function calls. Real Codex does this naturally;
# the deterministic synthetic loop must preserve the same wire contract.
reasoning = [item for item in response.get("output", []) if isinstance(item, dict) and item.get("type") == "reasoning"]
if reasoning:
    request["input"][1:1] = reasoning
request_path.write_text(json.dumps(request, separators=(",", ":")), encoding="utf-8")
PY
assert_live_response reanalysis "$TMP/reanalysis-request.json"

python3 - "$TMP/logs/deepseek-vision-trace" "$TMP/host.log" "$TMP/initial.out" "$TMP/reanalysis.out" <<'PY'
import json
import os
import pathlib
import stat
import sys

trace_root = pathlib.Path(sys.argv[1])
scan_paths = [pathlib.Path(item) for item in sys.argv[2:]]
host_log = scan_paths[0]
events_path = trace_root / "events.jsonl"
bundles = sorted((trace_root / "requests").iterdir())
assert len(bundles) == 2, bundles
assert stat.S_IMODE(trace_root.stat().st_mode) == 0o700
assert stat.S_IMODE(events_path.stat().st_mode) == 0o600

agent = []
for bundle in bundles:
    inbound = (bundle / "10-inbound-body.json").read_text(encoding="utf-8")
    if "live_view_001" in inbound and "live_reanalyze_001" in inbound:
        agent.append(bundle)
assert len(agent) == 1, agent
agent = agent[0]

rewritten = (agent / "50-rewritten-body.json").read_text(encoding="utf-8")
assert "Task-specific visual reanalysis" in rewritten, rewritten[:4000]
assert "data:image/" not in rewritten, rewritten[:4000]
assert '"type":"input_image"' not in rewritten, rewritten[:4000]
assert "live_view_001" in rewritten and "live_reanalyze_001" in rewritten

metadata_paths = [
    path for path in agent.glob("40-vlm-job-*-metadata.json")
    if not path.name.endswith("-request-metadata.json") and not path.name.endswith("-response-metadata.json")
]
metadata = "\n".join(path.read_text(encoding="utf-8") for path in metadata_paths)
assert "Inspect only the number of vertical bands" in metadata, metadata[:4000]
assert "Report only the four band colors" in metadata, metadata[:4000]
assert "data:image/png;base64," in metadata, metadata[:4000]

events = [json.loads(line) for line in events_path.read_text(encoding="utf-8").splitlines() if line.strip()]
assert sum(event.get("event") == "vlm_call_started" for event in events) >= 3, events
assert sum(event.get("event") == "vlm_fallback_selected" for event in events) == 3, events
if os.environ.get("LIVE_EXPECT_FALLBACK") == "1":
    attempts = [event for event in events if event.get("event") == "vlm_fallback_attempt"]
    assert len(attempts) >= 3, events
    selected = [event for event in events if event.get("event") == "vlm_fallback_selected"]
    assert all((event.get("data") or {}).get("attempt", 0) >= 2 for event in selected), selected
    primary_jobs = {
        (event.get("bundle"), (event.get("data") or {}).get("job_id"))
        for event in events
        if event.get("event") == "vlm_call_started" and (event.get("data") or {}).get("attempt") == 1
    }
    failed_jobs = {(event.get("bundle"), (event.get("data") or {}).get("job_id")) for event in attempts}
    selected_jobs = {(event.get("bundle"), (event.get("data") or {}).get("job_id")) for event in selected}
    assert len(primary_jobs) == 3, primary_jobs
    assert failed_jobs == primary_jobs, (failed_jobs, primary_jobs)
    assert selected_jobs == primary_jobs, (selected_jobs, primary_jobs)
    second_requests = [path for bundle in bundles for path in bundle.glob("40-vlm-job-*-attempt-02-*-request.json")]
    assert len(second_requests) >= 3, second_requests
    forced = os.environ.get("LIVE_FORCE_PRIMARY_STATUS")
    if forced:
        categories = {(event.get("data") or {}).get("category") for event in attempts}
        # CLIProxyAPI's host callback intentionally does not expose provider
        # transport details when execution fails. The plugin must not invent
        # the injected status even though the private fixture returned it.
        assert categories == {"host_executor_error"}, attempts
        assert all((event.get("data") or {}).get("upstream_status", 0) == 0 for event in attempts), attempts

for bundle in bundles:
    for path in bundle.iterdir():
        if path.is_file():
            assert stat.S_IMODE(path.stat().st_mode) == 0o600, (path, oct(path.stat().st_mode))
scan_paths.extend(path for path in trace_root.rglob("*") if path.is_file())
for path in scan_paths:
    raw = path.read_bytes()
    for name in ("DEEPSEEK_API_KEY", "GPT_API_KEY"):
        secret = os.environ.get(name, "").encode()
        assert not secret or secret not in raw, (path, name)
host_raw = host_log.read_bytes()
assert b"data:image/" not in host_raw, host_log
assert b".codex/attachments/att_live/" not in host_raw, host_log
PY
echo "deterministic live-provider loop and plaintext trace passed"

if [[ "$LIVE_USE_CODEX" == "1" ]]; then
  python3 - "$CODEX_HOME/config.toml" "$DEEPSEEK_MODEL" "$BASE/v1" <<'PY'
import json
import pathlib
import sys

path, model, base_url = sys.argv[1:]
text = f'''model = {json.dumps(model)}
model_provider = "live_proxy"
approval_policy = "never"
sandbox_mode = "read-only"

[model_providers.live_proxy]
name = "Temporary local CLIProxyAPI"
base_url = {json.dumps(base_url)}
env_key = "LIVE_PROXY_API_KEY"
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0

[history]
persistence = "none"
'''
pathlib.Path(path).write_text(text, encoding="utf-8")
PY
  chmod 600 "$CODEX_HOME/config.toml"
  echo "running optional genuine codex exec tool loop"
  CODEX_PROMPT="Before answering, you must call view_image on the attached image at $IMAGE_PATH. After the tool result, state only the vertical band colors from left to right."
  set +e
  env -u DEEPSEEK_API_KEY -u GPT_API_KEY \
    CODEX_HOME="$CODEX_HOME" LIVE_PROXY_API_KEY=live-client-key \
    timeout "$TIMEOUT" codex exec --ephemeral --json --skip-git-repo-check \
      -s read-only -C "$TMP/codex-work" "$CODEX_PROMPT" -i "$IMAGE_PATH" \
      >"$TMP/codex.jsonl" 2>"$TMP/codex.log"
  codex_rc=$?
  set -e
  if [[ "$codex_rc" != "0" ]]; then
    echo "codex exec failed with exit $codex_rc" >&2
    safe_log_excerpt "$TMP/codex.log" >&2
    exit 1
  fi
  python3 - "$TMP/logs/deepseek-vision-trace" "$TMP/codex.jsonl" "$TMP/codex.log" <<'PY'
import json
import os
import pathlib
import sys

trace_root = pathlib.Path(sys.argv[1])
codex_output = pathlib.Path(sys.argv[2])
codex_log = pathlib.Path(sys.argv[3])
assert codex_output.stat().st_size > 0, codex_output

matches = []
for inbound in (trace_root / "requests").glob("*/10-inbound-body.json"):
    raw = inbound.read_text(encoding="utf-8")
    if "live_view_001" in raw:
        continue
    payload = json.loads(raw)
    items = payload.get("input", []) if isinstance(payload, dict) else []
    has_view_call = any(
        isinstance(item, dict) and item.get("type") == "function_call" and item.get("name") == "view_image"
        for item in items
    )
    has_output = any(isinstance(item, dict) and item.get("type") == "function_call_output" for item in items)
    rewritten = inbound.parent / "50-rewritten-body.json"
    if has_view_call and has_output and rewritten.exists() and "Task-specific visual reanalysis" in rewritten.read_text(encoding="utf-8"):
        matches.append(inbound.parent)
assert matches, "codex did not produce a rich view_image tool-output reanalysis round"

events = [
    json.loads(line)
    for line in (trace_root / "events.jsonl").read_text(encoding="utf-8").splitlines()
    if line.strip()
]
if os.environ.get("LIVE_EXPECT_FALLBACK") == "1":
    primary_jobs = {
        (event.get("bundle"), (event.get("data") or {}).get("job_id"))
        for event in events
        if event.get("event") == "vlm_call_started" and (event.get("data") or {}).get("attempt") == 1
    }
    failed_jobs = {
        (event.get("bundle"), (event.get("data") or {}).get("job_id"))
        for event in events
        if event.get("event") == "vlm_fallback_attempt"
    }
    selected_jobs = {
        (event.get("bundle"), (event.get("data") or {}).get("job_id"))
        for event in events
        if event.get("event") == "vlm_fallback_selected" and (event.get("data") or {}).get("attempt", 0) >= 2
    }
    assert failed_jobs == primary_jobs, (failed_jobs, primary_jobs)
    assert selected_jobs == primary_jobs, (selected_jobs, primary_jobs)

for path in (codex_output, codex_log, *[item for item in trace_root.rglob("*") if item.is_file()]):
    raw = path.read_bytes()
    for name in ("DEEPSEEK_API_KEY", "GPT_API_KEY"):
        secret = os.environ.get(name, "").encode()
        assert not secret or secret not in raw, (path, name)
PY
  echo "genuine codex exec rich view_image loop passed"
fi

echo "live-provider E2E passed"
echo "warning: plaintext trace contained complete image and conversation data inside the disposable private directory"
