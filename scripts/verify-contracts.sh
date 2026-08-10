#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
export REPO_DIR="$repo_dir"

python3 - <<'PY'
import json
import os
from pathlib import Path

root = Path(os.environ["REPO_DIR"])
fixture_dir = root / "testdata" / "responses"
required_responses = {
    "01-content-input-image.json",
    "02-function-call-output.json",
    "03-multi-image-mixed.json",
    "04-no-image.json",
    "05-stream.json",
    "06-compact.json",
    "06-compact-rpc.json",
    "07-rpc-after-auth.json",
    "08-codex-view-image-reanalysis.json",
    "09-string-history-view-image.json",
}
actual = {p.name for p in fixture_dir.glob("*.json")}
missing = required_responses - actual
if missing:
    raise SystemExit(f"missing required fixtures: {sorted(missing)}")

protocol_fixtures = {
    root / "testdata" / "chat": {"01-user-image.json", "02-tool-image.json", "03-no-image.json", "04-string-history-image.json"},
    root / "testdata" / "claude": {"01-user-base64.json", "02-tool-result-url.json", "03-no-image.json", "04-string-history-image.json"},
}
for directory, required in protocol_fixtures.items():
    found = {p.name for p in directory.glob("*.json")}
    missing = required - found
    if missing:
        raise SystemExit(f"missing required fixtures in {directory.name}: {sorted(missing)}")

all_fixture_paths = list(fixture_dir.glob("*.json"))
for directory in protocol_fixtures:
    all_fixture_paths.extend(directory.glob("*.json"))
for path in sorted(all_fixture_paths):
    data = json.loads(path.read_text())
    if not isinstance(data, dict):
        raise SystemExit(f"fixture root must be object: {path}")
    text = path.read_text()
    for forbidden in ("sk-", "Authorization", "Bearer "):
        if forbidden in text:
            raise SystemExit(f"possible secret marker {forbidden!r} in {path}")

import base64
for rpc_path in (fixture_dir / "07-rpc-after-auth.json", fixture_dir / "06-compact-rpc.json"):
    rpc = json.loads(rpc_path.read_text())
    try:
        decoded = base64.b64decode(rpc["Body"], validate=True)
        if not isinstance(json.loads(decoded), dict):
            raise ValueError("decoded body is not an object")
    except Exception as exc:
        raise SystemExit(f"invalid base64 RPC body in {rpc_path.name}: {exc}")
compact = json.loads((fixture_dir / "06-compact-rpc.json").read_text())
if compact.get("Metadata", {}).get("request_path") != "/v1/responses/compact":
    raise SystemExit("compact RPC fixture must carry /v1/responses/compact metadata")

contract = (root / "docs" / "contracts.md").read_text()
for marker in (
    "ABI 版本：`1`",
    "schema 版本：`2`",
    "`openai-response` + `/v1/responses`",
    "openai` + `/v1/chat/completions",
    "claude` + `/v1/messages",
    "request.intercept_after",
    "deepseek-v4-flash",
    "deepseek-v4-pro",
    "gpt-5.6-luna",
    "max_output_tokens",
    "StatusCode=502",
):
    if marker not in contract:
        raise SystemExit(f"contract marker missing: {marker}")
for forbidden in ("request.complete", "request_lifecycle_plugin", '"ToFormat": "openai-response"', '"temperature":0'):
    if forbidden in contract:
        raise SystemExit(f"stale contract marker present: {forbidden}")
for rpc_name in ("07-rpc-after-auth.json", "06-compact-rpc.json"):
    rpc = json.loads((fixture_dir / rpc_name).read_text())
    if rpc.get("ToFormat") != "openai":
        raise SystemExit(f"{rpc_name} must demonstrate a host-selected non-frozen ToFormat")
print(f"verified {len(all_fixture_paths)} JSON fixtures and required contract markers")
PY
