#!/usr/bin/env bash
# End-to-end test through a real CLIProxyAPI process and its c-shared loader.
# No external service, Docker, or credentials are required.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CLI_ROOT=${CLIPROXY_ROOT:-}
TIMEOUT=${E2E_TIMEOUT_SECONDS:-180}
TMP=$(mktemp -d "${TMPDIR:-/tmp}/deepseek-vision-host-e2e.XXXXXX")
PLUGIN_DIR="$TMP/plugins"
mkdir -p "$PLUGIN_DIR/linux/amd64" "$TMP/auth"

HOST_PID=""
VLM_PID=""
UPSTREAM_PID=""
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  for pid in "$HOST_PID" "$VLM_PID" "$UPSTREAM_PID"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  for pid in "$HOST_PID" "$VLM_PID" "$UPSTREAM_PID"; do
    if [[ -n "$pid" ]]; then
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [[ "${KEEP_E2E_TMP:-}" == "1" ]]; then
    echo "host-e2e temporary directory preserved: $TMP" >&2
  else
    rm -rf "$TMP"
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

if ! command -v curl >/dev/null || ! command -v python3 >/dev/null; then
  echo "host-e2e requires curl and python3" >&2
  exit 2
fi
if [[ -z "$CLI_ROOT" || ! -d "$CLI_ROOT" ]]; then
  echo "set CLIPROXY_ROOT to a CLIProxyAPI checkout (v7.2.119)" >&2
  exit 2
fi

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
VLM_PORT=$(free_port)
UPSTREAM_PORT=$(free_port)
VLM_STATE="$TMP/vlm-state.json"
UPSTREAM_STATE="$TMP/upstream-state.json"
VLM_MODE="$TMP/vlm-mode"
printf 'ok\n' >"$VLM_MODE"

python3 "$ROOT/test/e2e/mock_openai.py" --port "$VLM_PORT" --role vlm --state "$VLM_STATE" --mode-file "$VLM_MODE" >"$TMP/vlm.log" 2>&1 &
VLM_PID=$!
python3 "$ROOT/test/e2e/mock_openai.py" --port "$UPSTREAM_PORT" --role upstream --state "$UPSTREAM_STATE" >"$TMP/upstream.log" 2>&1 &
UPSTREAM_PID=$!

deadline=$((SECONDS + TIMEOUT))
until curl -fsS --max-time 1 "http://127.0.0.1:$VLM_PORT/healthz" >/dev/null 2>&1 && \
      curl -fsS --max-time 1 "http://127.0.0.1:$UPSTREAM_PORT/healthz" >/dev/null 2>&1; do
  (( SECONDS < deadline )) || { echo "mock server startup timed out" >&2; exit 1; }
  sleep 0.1
done

echo "building plugin and CLIProxyAPI host"
CGO_ENABLED=1 GOTOOLCHAIN=auto go build -buildmode=c-shared -trimpath \
  -ldflags='-s -w -X main.pluginVersion=0.3.0-host-e2e' \
  -o "$PLUGIN_DIR/linux/amd64/deepseek-vision-v0.3.0.so" "$ROOT"
(cd "$CLI_ROOT" && CGO_ENABLED=1 GOTOOLCHAIN=auto go build -trimpath -o "$TMP/cliproxy" ./cmd/server)

CONFIG="$TMP/config.yaml"
python3 - "$CONFIG" "$PLUGIN_DIR" "$HOST_PORT" "$VLM_PORT" "$UPSTREAM_PORT" <<'PY'
import pathlib
import sys

path, plugin_dir, host_port, vlm_port, upstream_port = sys.argv[1:]
text = f'''host: 127.0.0.1
port: {host_port}
debug: true
auth-dir: "{pathlib.Path(path).parent / 'auth'}"
api-keys:
  - client-key
remote-management:
  allow-remote: false
  secret-key: management-key
  disable-control-panel: true
plugins:
  enabled: true
  dir: "plugins"
  configs:
    deepseek-vision:
      enabled: true
      priority: 100
      store:
        source: "host-e2e"
        version: "0.3.0"
      target_models: [deepseek-v4-flash]
      vision_model: "gpt-5.6-luna"
      vision_fallback_models: []
      agent_reanalysis_enabled: true
      language: "en"
      request_timeout_seconds: 10
      max_inflight_vision_requests: 4
      emergency_max_images_per_request: 256
      max_request_bytes: 20971520
      max_image_reference_bytes: 1048576
      max_response_bytes: 1048576
      max_result_chars: 2000
      analysis_cache_size: 16
      analysis_cache_ttl_seconds: 60
      analysis_url_cache_ttl_seconds: 30
      trace_enabled: true
openai-compatibility:
  - name: vision-e2e
    base-url: "http://127.0.0.1:{vlm_port}/v1"
    api-key-entries:
      - api-key: vision-host-key
    models:
      - name: gpt-5.6-luna
        alias: gpt-5.6-luna
  - name: host-e2e
    base-url: "http://127.0.0.1:{upstream_port}/v1"
    api-key-entries:
      - api-key: upstream-key
    models:
      - name: deepseek-v4-flash
        alias: deepseek-v4-flash
        input-modalities: [text, image]
      - name: deepseek-v4-flash
        alias: deepseek-flash-alias
        input-modalities: [text, image]
      - name: other-model
        alias: other-model
        input-modalities: [text, image]
'''
pathlib.Path(path).write_text(text, encoding="utf-8")
PY

(cd "$TMP" && "$TMP/cliproxy" -config "$CONFIG" -local-model >"$TMP/host.log" 2>&1) &
HOST_PID=$!
BASE="http://127.0.0.1:$HOST_PORT"

deadline=$((SECONDS + TIMEOUT))
until curl -fsS --max-time 2 -H 'Authorization: Bearer client-key' "$BASE/v1/models" >/dev/null 2>&1; do
  if ! kill -0 "$HOST_PID" 2>/dev/null; then
    echo "CLIProxyAPI exited during startup" >&2
    sed -n '1,160p' "$TMP/host.log" >&2 || true
    exit 1
  fi
  (( SECONDS < deadline )) || { echo "CLIProxyAPI startup timed out" >&2; exit 1; }
  sleep 0.2
done

# The management secret comes from config.yaml. Keep the process alive beyond
# CLIProxyAPI's short-lived password-flag mode so slow VLM calls cannot race a
# self-termination regression.
sleep 11
if ! kill -0 "$HOST_PID" 2>/dev/null; then
  echo "CLIProxyAPI exited before the post-startup liveness check" >&2
  sed -n '1,160p' "$TMP/host.log" >&2 || true
  exit 1
fi

mgmt() {
  curl -fsS --max-time 4 -H 'Authorization: Bearer management-key' "$@"
}
state_json() {
  curl -fsS --max-time 3 "$1/state"
}
count_role() {
  python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("requests", [])))'
}
VLM_BEFORE=$(state_json "http://127.0.0.1:$VLM_PORT" | count_role)
UPSTREAM_BEFORE=$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)

mgmt "$BASE/v0/management/plugins" >"$TMP/plugins.json"
python3 - "$TMP/plugins.json" <<'PY'
import json, sys
payload = json.load(open(sys.argv[1], encoding="utf-8"))
assert payload["plugins_enabled"] is True
items = {x["id"]: x for x in payload["plugins"]}
item = items["deepseek-vision"]
assert item["registered"] is True, item
assert item["effective_enabled"] is True, item
assert item["configured"] is True, item
fields = {field["name"]: field["type"] for field in item["config_fields"]}
assert len(fields) == 12, fields
assert fields["target_models"] == "array", fields
assert fields["vision_model"] == "string", fields
assert fields["vision_fallback_models"] == "array", fields
assert fields["language"] == "enum", fields
assert fields["max_inflight_vision_requests"] == "integer", fields
assert fields["emergency_max_images_per_request"] == "integer", fields
assert fields["request_timeout_seconds"] == "integer", fields
assert fields["analysis_cache_size"] == "integer", fields
assert fields["analysis_cache_ttl_seconds"] == "integer", fields
assert fields["analysis_url_cache_ttl_seconds"] == "integer", fields
assert fields["agent_reanalysis_enabled"] == "boolean", fields
assert fields["trace_enabled"] == "boolean", fields
PY
mgmt "$BASE/v0/management/plugins/deepseek-vision/config" >"$TMP/plugin-config.json"
python3 - "$TMP/plugin-config.json" <<'PY'
import json, sys
payload = json.load(open(sys.argv[1], encoding="utf-8"))
assert payload.get("store", {}).get("source") == "host-e2e", payload
assert payload.get("store", {}).get("version") == "0.3.0", payload
assert payload.get("analysis_cache_size") == 16, payload
assert payload.get("analysis_cache_ttl_seconds") == 60, payload
assert payload.get("analysis_url_cache_ttl_seconds") == 30, payload
assert payload.get("max_inflight_vision_requests") == 4, payload
assert payload.get("emergency_max_images_per_request") == 256, payload
assert payload.get("target_models") == ["deepseek-v4-flash"], payload
assert payload.get("vision_fallback_models") == [], payload
assert payload.get("agent_reanalysis_enabled") is True, payload
PY
echo "plugin registered/effective and store metadata accepted"

# A user can leave an incomplete edit without making the plugin disappear from
# the management snapshot. The last known-good runtime remains active.
mgmt -X PATCH -H 'Content-Type: application/json' \
  -d '{"vision_model":null}' \
  "$BASE/v0/management/plugins/deepseek-vision/config" >/dev/null
mgmt "$BASE/v0/management/plugins" | python3 -c '
import json,sys
item=next(x for x in json.load(sys.stdin)["plugins"] if x["id"]=="deepseek-vision")
assert item["registered"] is True and item["effective_enabled"] is True, item
'
mgmt -X PATCH -H 'Content-Type: application/json' \
  -d '{"vision_model":"gpt-5.6-luna"}' \
  "$BASE/v0/management/plugins/deepseek-vision/config" >/dev/null
echo "incomplete edit preserved registration and last known-good runtime"

post() {
  local path=$1
  local body=$2
  local out=$3
  curl -sS --max-time 20 -o "$out" -w '%{http_code}' \
    -H 'Authorization: Bearer client-key' -H 'Content-Type: application/json' \
    -X POST "$BASE$path" --data-binary "$body"
}

make_request() {
  local model=$1 stream=$2 suffix=$3
  python3 - "$model" "$stream" "$suffix" <<'PY'
import base64, json, sys
model, stream, suffix = sys.argv[1], sys.argv[2] == "true", sys.argv[3]
image = "data:image/png;base64," + base64.b64encode(("e2e-" + suffix).encode()).decode()
print(json.dumps({"model": model, "input": [{"role": "user", "content": [
    {"type": "input_text", "text": "Describe this " + suffix},
    {"type": "input_image", "image_url": image}
]}], "stream": stream}, separators=(",", ":")) )
PY
}

make_chat_request() {
  local model=$1 stream=$2 suffix=$3
  python3 - "$model" "$stream" "$suffix" <<'PY'
import base64, json, sys
model, stream, suffix = sys.argv[1], sys.argv[2] == "true", sys.argv[3]
image = "data:image/png;base64," + base64.b64encode(("chat-" + suffix).encode()).decode()
print(json.dumps({"model": model, "messages": [{"role": "user", "content": [
    {"type": "text", "text": "Describe this " + suffix},
    {"type": "image_url", "image_url": {"url": image}}
]}], "stream": stream}, separators=(",", ":")))
PY
}

make_claude_request() {
  local model=$1 stream=$2 suffix=$3 include_tool=${4:-false}
  python3 - "$model" "$stream" "$suffix" "$include_tool" <<'PY'
import base64, json, sys
model, stream, suffix, include_tool = sys.argv[1], sys.argv[2] == "true", sys.argv[3], sys.argv[4] == "true"
content = [
    {"type": "text", "text": "Describe this " + suffix},
    {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": base64.b64encode(("claude-" + suffix).encode()).decode()}},
]
if include_tool:
    content.append({"type": "tool_result", "tool_use_id": "tool_e2e", "content": [
        {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": base64.b64encode(("tool-" + suffix).encode()).decode()}}
    ]})
print(json.dumps({"model": model, "max_tokens": 256, "messages": [{"role": "user", "content": content}], "stream": stream}, separators=(",", ":")))
PY
}

assert_success() {
  local label=$1 path=$2 body=$3 expected_vlm=$4 expected_upstream=$5
  local out="$TMP/$label.out"
  local code
  code=$(post "$path" "$body" "$out")
  [[ "$code" == "200" ]] || { echo "$label failed HTTP $code: $(head -c 500 "$out")" >&2; exit 1; }
  local vlm_count upstream_count
  vlm_count=$(state_json "http://127.0.0.1:$VLM_PORT" | count_role)
  upstream_count=$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)
  [[ "$vlm_count" == "$expected_vlm" ]] || { echo "$label VLM count=$vlm_count want=$expected_vlm" >&2; exit 1; }
  [[ "$upstream_count" == "$expected_upstream" ]] || { echo "$label upstream count=$upstream_count want=$expected_upstream" >&2; exit 1; }
}

assert_last_upstream_rewritten() {
  python3 - "$UPSTREAM_STATE" <<'PY'
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
body = state["requests"][-1].get("body") or {}
raw = json.dumps(body, ensure_ascii=False)
assert "input_image" not in raw, raw[:1000]
assert "image_url" not in raw, raw[:1000]
assert '"type": "image"' not in raw, raw[:1000]
assert "data:image/" not in raw, raw[:1000]
assert "Visual analysis" in raw or "Visible text" in raw, raw[:1000]
PY
}

assert_last_upstream_has_image() {
  python3 - "$UPSTREAM_STATE" <<'PY'
import json, sys
state = json.load(open(sys.argv[1], encoding="utf-8"))
body = state["requests"][-1].get("body") or {}
raw = json.dumps(body, ensure_ascii=False)
assert "data:image/" in raw or "input_image" in raw or "image_url" in raw, raw[:1000]
PY
}

body=$(make_request deepseek-v4-flash false direct)
assert_success direct /v1/responses "$body" "$((VLM_BEFORE + 1))" "$((UPSTREAM_BEFORE + 1))"
assert_last_upstream_rewritten

# The identical request is rewritten from the generation-local cache and does
# not make another VLM call.
assert_success cache-hit /v1/responses "$body" "$((VLM_BEFORE + 1))" "$((UPSTREAM_BEFORE + 2))"
assert_last_upstream_rewritten

body=$(make_request deepseek-flash-alias false alias)
assert_success alias /v1/responses "$body" "$((VLM_BEFORE + 2))" "$((UPSTREAM_BEFORE + 3))"
assert_last_upstream_rewritten

body=$(make_request deepseek-v4-flash true stream)
assert_success stream /v1/responses "$body" "$((VLM_BEFORE + 3))" "$((UPSTREAM_BEFORE + 4))"
grep -q 'data:' "$TMP/stream.out" || { echo "stream response did not contain SSE" >&2; exit 1; }

multi_body=$(python3 - <<'PY'
import base64, json
def image(n):
    return "data:image/png;base64," + base64.b64encode(("multi-" + str(n)).encode()).decode()
print(json.dumps({"model":"deepseek-v4-flash","input":[
 {"role":"user","content":[{"type":"input_image","image_url":image(1)},{"type":"input_image","image_url":image(2)}]},
 {"type":"function_call_output","call_id":"e2e","output":[{"type":"input_image","image_url":image(3)}]}
],"stream":False}, separators=(",", ":")))
PY
)
assert_success multi /v1/responses "$multi_body" "$((VLM_BEFORE + 5))" "$((UPSTREAM_BEFORE + 5))"
assert_last_upstream_rewritten

agent_body=$(python3 - <<'PY'
import base64, json
def image(n):
    return "data:image/png;base64," + base64.b64encode(("agent-" + str(n)).encode()).decode()
print(json.dumps({"model":"deepseek-v4-flash","tools":[
 {"type":"function","name":"view_image"},
 {"type":"function","name":"deepseek_vision_reanalyze"}
],"input":[
 {"role":"user","content":[{"type":"input_text","text":"Inspect spacing and small labels"}]},
 {"type":"function_call","name":"view_image","call_id":"view_e2e","arguments":"{\"path\":\"/tmp/.codex/attachments/att_e2e/shot.png\",\"detail\":\"high\"}"},
 {"type":"function_call_output","call_id":"view_e2e","output":[{"type":"input_image","image_url":image(1)}]},
 {"type":"function_call","name":"deepseek_vision_reanalyze","call_id":"reanalyze_e2e","arguments":json.dumps({"attachment_ids":["att_e2e"],"focus":"Read only the small labels","detail":"original","cache":"no_store"})},
 {"type":"function_call_output","call_id":"reanalyze_e2e","output":[{"type":"input_image","image_url":image(2)}]}
],"stream":False}, separators=(",", ":")))
PY
)
assert_success agent-tools /v1/responses "$agent_body" "$((VLM_BEFORE + 7))" "$((UPSTREAM_BEFORE + 6))"
assert_last_upstream_rewritten
grep -q 'Task-specific visual reanalysis' "$UPSTREAM_STATE" || { echo "agent reanalysis marker missing from upstream request" >&2; exit 1; }

# Full-context trace is deliberately high sensitivity. This fixture keeps it in
# the private disposable E2E directory and verifies the complete request/VLM/
# rewrite chain without copying any artifact into the repository.
python3 - "$TMP/logs/deepseek-vision-trace" <<'PY'
import json
import pathlib
import stat
import sys
from collections import Counter

root = pathlib.Path(sys.argv[1])
events_path = root / "events.jsonl"
assert root.is_dir(), root
assert events_path.is_file(), events_path
assert stat.S_IMODE(root.stat().st_mode) == 0o700, oct(root.stat().st_mode)
assert stat.S_IMODE(events_path.stat().st_mode) == 0o600, oct(events_path.stat().st_mode)

matches = []
for inbound in (root / "requests").glob("*/10-inbound-body.json"):
    raw = inbound.read_text(encoding="utf-8")
    if "view_e2e" in raw and "reanalyze_e2e" in raw:
        matches.append(inbound.parent)
assert len(matches) == 1, matches
bundle = matches[0]

required = [
    "00-metadata.json", "10-inbound-body.json", "20-discovery.json",
    "30-cache-plan.json", "50-rewritten-body.json", "90-result.json",
]
for name in required:
    path = bundle / name
    assert path.is_file(), path
    assert stat.S_IMODE(path.stat().st_mode) == 0o600, (path, oct(path.stat().st_mode))

vlm_metadata = sorted(
    path for path in bundle.glob("40-vlm-job-*-metadata.json")
    if not path.name.endswith("-request-metadata.json") and not path.name.endswith("-response-metadata.json")
)
vlm_requests = sorted(path for path in bundle.glob("40-vlm-job-*-request.json") if "-reasoning-" not in path.name)
vlm_responses = sorted(path for path in bundle.glob("40-vlm-job-*-response.json") if "-reasoning-" not in path.name)
vlm_results = sorted(bundle.glob("40-vlm-job-*-parsed-result.txt"))
assert len(vlm_metadata) == len(vlm_requests) == len(vlm_responses) == len(vlm_results) == 2, (
    vlm_metadata, vlm_requests, vlm_responses, vlm_results
)

metadata_text = "\n".join(path.read_text(encoding="utf-8") for path in vlm_metadata)
request_text = "\n".join(path.read_text(encoding="utf-8") for path in vlm_requests)
assert "Inspect spacing and small labels" in metadata_text, metadata_text[:2000]
assert "Read only the small labels" in metadata_text, metadata_text[:2000]
assert "data:image/png;base64," in request_text, request_text[:2000]

inbound_text = (bundle / "10-inbound-body.json").read_text(encoding="utf-8")
rewritten_text = (bundle / "50-rewritten-body.json").read_text(encoding="utf-8")
assert "data:image/png;base64," in inbound_text, inbound_text[:2000]
assert "data:image/" not in rewritten_text, rewritten_text[:2000]
assert '"type":"input_image"' not in rewritten_text, rewritten_text[:2000]
assert "Task-specific visual reanalysis" in rewritten_text, rewritten_text[:2000]
assert "view_e2e" in rewritten_text and "reanalyze_e2e" in rewritten_text, rewritten_text[:2000]

events = [json.loads(line) for line in events_path.read_text(encoding="utf-8").splitlines() if line.strip()]
bundle_name = "requests/" + bundle.name
names = [event["event"] for event in events if event.get("bundle") == bundle_name]
assert names.count("vlm_call_started") == 2, names
assert names.count("vlm_call_finished") == 2, names
assert names.count("vlm_fallback_selected") == 2, names
assert names.count("rewrite_completed") == 1, names
assert names.count("interceptor_completed") == 1, names
bundle_events = [event for event in events if event.get("bundle") == bundle_name]
started_jobs = Counter((event.get("data") or {}).get("job_id") for event in bundle_events if event.get("event") == "vlm_call_started")
selected_jobs = Counter((event.get("data") or {}).get("job_id") for event in bundle_events if event.get("event") == "vlm_fallback_selected")
assert started_jobs == Counter({1: 1, 2: 1}), started_jobs
assert selected_jobs == started_jobs, (started_jobs, selected_jobs)
PY
echo "plaintext trace captured and verified for rich tool-output reanalysis"

no_image='{"model":"deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_text","text":"plain"}]}],"stream":false}'
assert_success no-image /v1/responses "$no_image" "$((VLM_BEFORE + 7))" "$((UPSTREAM_BEFORE + 7))"

non_target=$(make_request other-model false non-target)
assert_success non-target /v1/responses "$non_target" "$((VLM_BEFORE + 7))" "$((UPSTREAM_BEFORE + 8))"

compact=$(make_request deepseek-v4-flash false compact)
code=$(post /v1/responses/compact "$compact" "$TMP/compact.out")
[[ "$code" == "200" ]] || { echo "compact failed HTTP $code" >&2; exit 1; }
vlm_count=$(state_json "http://127.0.0.1:$VLM_PORT" | count_role)
upstream_count=$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)
[[ "$vlm_count" == "$((VLM_BEFORE + 7))" ]] || { echo "compact was not bypassed" >&2; exit 1; }
[[ "$upstream_count" == "$((UPSTREAM_BEFORE + 9))" ]] || { echo "compact upstream count=$upstream_count" >&2; exit 1; }

chat_body=$(make_chat_request deepseek-v4-flash true chat-stream)
assert_success chat-stream /v1/chat/completions "$chat_body" "$((VLM_BEFORE + 8))" "$((UPSTREAM_BEFORE + 10))"
grep -q 'data:' "$TMP/chat-stream.out" || { echo "chat stream response did not contain SSE" >&2; exit 1; }
assert_last_upstream_rewritten

claude_body=$(make_claude_request deepseek-v4-flash false claude-tools true)
assert_success claude /v1/messages "$claude_body" "$((VLM_BEFORE + 10))" "$((UPSTREAM_BEFORE + 11))"
assert_last_upstream_rewritten
python3 - "$TMP/claude.out" <<'PY'
import json, sys
payload = json.load(open(sys.argv[1], encoding="utf-8"))
assert payload.get("type") == "message", payload
PY

printf 'fail\n' >"$VLM_MODE"
failure=$(make_request deepseek-v4-flash false vlm-failure)
code=$(post /v1/responses "$failure" "$TMP/failure.out")
[[ "$code" == "502" ]] || { echo "VLM failure HTTP $code, want 502" >&2; exit 1; }
grep -q 'vision_preprocess_error' "$TMP/failure.out" || { echo "failure response was not sanitized" >&2; exit 1; }
grep -q 'vision_fallback_exhausted' "$TMP/failure.out" || { echo "failure response omitted fallback code" >&2; exit 1; }
grep -q 'error_id' "$TMP/failure.out" || { echo "failure response omitted correlation ID" >&2; exit 1; }
[[ "$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)" == "$((UPSTREAM_BEFORE + 11))" ]] || { echo "upstream called after Responses VLM failure" >&2; exit 1; }

chat_failure=$(make_chat_request deepseek-v4-flash false chat-failure)
code=$(post /v1/chat/completions "$chat_failure" "$TMP/chat-failure.out")
[[ "$code" == "502" ]] || { echo "Chat VLM failure HTTP $code, want 502" >&2; exit 1; }
grep -q 'vision_preprocess_error' "$TMP/chat-failure.out" || { echo "Chat failure response was not OpenAI-shaped" >&2; exit 1; }
grep -q 'vision_fallback_exhausted' "$TMP/chat-failure.out" || { echo "Chat failure response omitted fallback code" >&2; exit 1; }
[[ "$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)" == "$((UPSTREAM_BEFORE + 11))" ]] || { echo "upstream called after Chat VLM failure" >&2; exit 1; }

claude_failure=$(make_claude_request deepseek-v4-flash false claude-failure false)
code=$(post /v1/messages "$claude_failure" "$TMP/claude-failure.out")
[[ "$code" == "502" ]] || { echo "Claude VLM failure HTTP $code, want 502" >&2; exit 1; }
python3 - "$TMP/claude-failure.out" <<'PY'
import json, sys
payload = json.load(open(sys.argv[1], encoding="utf-8"))
assert payload.get("type") == "error", payload
assert payload.get("error", {}).get("type") == "api_error", payload
assert payload.get("error", {}).get("code") == "vision_fallback_exhausted", payload
assert payload.get("error", {}).get("details", {}).get("error_id", "").startswith("vpe_"), payload
PY
[[ "$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)" == "$((UPSTREAM_BEFORE + 11))" ]] || { echo "upstream called after Claude VLM failure" >&2; exit 1; }
printf 'ok\n' >"$VLM_MODE"

unsupported='{"model":"deepseek-v4-flash","input":[{"role":"user","content":[{"type":"input_image","file_id":"file_unsupported"}]}],"stream":false}'
code=$(post /v1/responses "$unsupported" "$TMP/unsupported.out")
[[ "$code" == "422" ]] || { echo "unsupported image source HTTP $code, want 422" >&2; exit 1; }
[[ "$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)" == "$((UPSTREAM_BEFORE + 11))" ]] || { echo "upstream called for unsupported image" >&2; exit 1; }

unsupported_claude='{"model":"deepseek-v4-flash","max_tokens":64,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"file","file_id":"file_unsupported"}}]}]}'
code=$(post /v1/messages "$unsupported_claude" "$TMP/unsupported-claude.out")
[[ "$code" == "422" ]] || { echo "unsupported Claude image source HTTP $code, want 422" >&2; exit 1; }
grep -q 'invalid_request_error' "$TMP/unsupported-claude.out" || { echo "unsupported Claude error was not protocol native" >&2; exit 1; }
[[ "$(state_json "http://127.0.0.1:$UPSTREAM_PORT" | count_role)" == "$((UPSTREAM_BEFORE + 11))" ]] || { echo "upstream called for unsupported Claude image" >&2; exit 1; }

# A provider failure can put the host's VLM auth into cooldown. Subsequent
# protocol checks may therefore fail before another mock VLM HTTP call. Capture
# the observed count instead of assuming every sanitized 502 crossed the
# provider transport boundary; the invariant above is zero business-upstream
# calls for all three failed preprocessing requests.
VLM_AFTER_FAILURES=$(state_json "http://127.0.0.1:$VLM_PORT" | count_role)

mgmt -X PATCH -H 'Content-Type: application/json' -d '{"enabled":false}' "$BASE/v0/management/plugins/deepseek-vision/enabled" >/dev/null
deadline=$((SECONDS + 20))
until [[ "$(mgmt "$BASE/v0/management/plugins" | python3 -c 'import json,sys; print(next(x for x in json.load(sys.stdin)["plugins"] if x["id"]=="deepseek-vision")["effective_enabled"])')" == "False" ]]; do
  (( SECONDS < deadline )) || { echo "plugin disable did not settle" >&2; exit 1; }
  sleep 0.2
done
disabled=$(make_request deepseek-v4-flash false disabled)
assert_success disabled /v1/responses "$disabled" "$VLM_AFTER_FAILURES" "$((UPSTREAM_BEFORE + 12))"
mgmt -X PATCH -H 'Content-Type: application/json' -d '{"enabled":true}' "$BASE/v0/management/plugins/deepseek-vision/enabled" >/dev/null
deadline=$((SECONDS + 20))
until [[ "$(mgmt "$BASE/v0/management/plugins" | python3 -c 'import json,sys; print(next(x for x in json.load(sys.stdin)["plugins"] if x["id"]=="deepseek-vision")["effective_enabled"])')" == "True" ]]; do
  (( SECONDS < deadline )) || { echo "plugin re-enable did not settle" >&2; exit 1; }
  sleep 0.2
done
reloaded=$(make_request deepseek-v4-flash false reloaded)
assert_success reloaded /v1/responses "$reloaded" "$((VLM_AFTER_FAILURES + 1))" "$((UPSTREAM_BEFORE + 13))"

# Exercise the Management array field through persistence and runtime gating,
# not just registration metadata. The former target must pass through with its
# image intact, while the newly selected target must invoke vision rewriting.
mgmt -X PATCH -H 'Content-Type: application/json' \
  -d '{"target_models":["other-model"]}' \
  "$BASE/v0/management/plugins/deepseek-vision/config" >/dev/null
deadline=$((SECONDS + 20))
until mgmt "$BASE/v0/management/plugins/deepseek-vision/config" | python3 -c '
import json,sys
raise SystemExit(0 if json.load(sys.stdin).get("target_models") == ["other-model"] else 1)
'; do
  (( SECONDS < deadline )) || { echo "target_models config update did not settle" >&2; exit 1; }
  sleep 0.2
done

old_target=$(make_request deepseek-v4-flash false old-target-bypass)
assert_success old-target-bypass /v1/responses "$old_target" "$((VLM_AFTER_FAILURES + 1))" "$((UPSTREAM_BEFORE + 14))"
assert_last_upstream_has_image

new_target=$(make_request other-model false new-target-intercept)
assert_success new-target-intercept /v1/responses "$new_target" "$((VLM_AFTER_FAILURES + 2))" "$((UPSTREAM_BEFORE + 15))"
assert_last_upstream_rewritten

mgmt -X PATCH -H 'Content-Type: application/json' \
  -d '{"target_models":["deepseek-v4-flash"]}' \
  "$BASE/v0/management/plugins/deepseek-vision/config" >/dev/null
mgmt "$BASE/v0/management/plugins/deepseek-vision/config" | python3 -c '
import json,sys
assert json.load(sys.stdin).get("target_models") == ["deepseek-v4-flash"]
'

if grep -E 'fixture-vlm-key|upstream-key|data:image/' "$TMP/host.log" >/dev/null 2>&1; then
  echo "sensitive credential or image reference leaked to host log" >&2
  exit 1
fi

echo "host process/dynamic-loader E2E passed"
echo "covered: registration, effective status, store/cache metadata, Management target_models persistence/runtime gating, Responses/Chat/Claude routes, cache hit, direct/alias/stream, view_image/dedicated rich tool-output reanalysis, content/tool-result/multi-image rewrite, compact/non-target/no-image bypass, protocol-native structured VLM fail-closed 502, unsupported 422, disable/re-enable reload, log redaction"
echo "bounded gaps: management delete/unload and 413 oversized callback are not asserted; host management delete removes the fixture library and cannot be safely restored within one process run"
