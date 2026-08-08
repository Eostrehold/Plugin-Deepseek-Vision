# Troubleshooting

## Plugin is not discovered

Check the file name and platform directory. Manual mode must contain exactly
one unversioned candidate. CLIProxyAPI uses `<GOOS>/<GOARCH>` directories and
the platform extension (`.so` on Linux, `.dylib` on macOS, `.dll` on Windows).
The commands below are a Linux amd64 example:

```text
<CLI_PROXY_PLUGIN_PATH>/linux/amd64/deepseek-vision.so
```

The basename must be `deepseek-vision.so` in manual mode. Confirm the host and
artifact architecture with `file`, verify the ABI symbol, and check for stale
versioned candidates:

```bash
file plugins/linux/amd64/deepseek-vision.so
nm -D plugins/linux/amd64/deepseek-vision.so | grep cliproxy_plugin_init
test "$(find plugins/linux/amd64 -maxdepth 1 -type f -name 'deepseek-vision*.so' | wc -l)" -eq 1
```

For store/versioned mode, the expected filename is
`deepseek-vision-vX.Y.Z.so`; set `plugins.configs.deepseek-vision.store.version`
to the same version and inspect the management API. Its `path` must point to
that versioned file and `metadata.version` must match `X.Y.Z`:

```bash
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins \
  | jq '.plugins[] | select(.id == "deepseek-vision") | {path, registered, effective_enabled, metadata}'
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins/deepseek-vision/config \
  | jq '{store, enabled, priority}'
```

Restart the host after replacing a dynamic library and inspect its plugin
registration log. Ensure the host's plugin directory is not mounted read-only
for a deployment that performs plugin-store installation.

If a manual install has both `deepseek-vision.so` and
`deepseek-vision-v*.so`, remove the versioned candidates and restart. If a
store install has multiple versions, the pinned `store.version` must match an
existing artifact; reinstall the requested archive if the host cleaned up an
unselected old file.

## CPAMC shows no configurable fields

CLIProxyAPI exposes `config_fields` only after a plugin has registered. A
discovered but disabled or unconfigured binary can therefore show the generic
"no declared visual configuration fields" message before its first load.

For first-time CPAMC setup, enable the plugin and save once, wait until its
status becomes `registered`, then reopen the configuration drawer. Invalid or
incomplete edits never return a lifecycle error to the host: the plugin stays
registered and keeps the last known-good runtime, or remains safely unavailable
when no valid generation exists. Correct the field and save again; restarting
CLIProxyAPI is not required.

Verify what the host received from the plugin with:

```bash
curl -fsS -H 'Authorization: Bearer <management-key>' \
  http://127.0.0.1:8085/v0/management/plugins \
  | jq '.plugins[]
      | select(.id == "deepseek-vision")
      | {registered, effective_enabled, config_fields}'
```

A current binary reports twelve fields: target models, primary model, ordered
fallback models, language, global in-flight vision requests, emergency image
ceiling, total timeout, three cache controls, the agent reanalysis switch, and
the plaintext trace switch.
If `registered` remains `false`, restart
CLIProxyAPI after replacing the library and inspect the plugin registration
error in the host log; an old binary or failed ABI load cannot publish field
metadata.

## Requests pass through unexpectedly

The plugin handles the exact `/v1/responses` (`openai-response`),
`/v1/chat/completions` (`openai`), and `/v1/messages` (`claude`) route pairs for
final models listed in `target_models`. An alias is
checked after host resolution; `RequestedModel` alone is not sufficient.
`/v1/responses/compact`, `/v1/messages/count_tokens`, other APIs, non-target
models and requests without images are expected pass-through cases. If a request
uses `previous_response_id`, remember that server-side history remains hidden
from this callback; only images present in the current visible `input[]` can be
rewritten.

For v0.3.0, use `deepseek-v4-flash` when checking a real upstream. The
`deepseek-v4-pro` entry is future-supported configuration only; it is not a
required or probed service in this release.

## HTTP 502 from image requests

Check that `vision_model` and any ordered `vision_fallback_models` exist in
CLIProxyAPI and accept image input through OpenAI Responses. Fallback advances
only for HTTP 408/429/5xx, an attempt timeout, invalid/empty/oversized output,
or a generic host executor error. Parent timeout/cancellation and rewrite
failures stop immediately. Provider errors, malformed VLM JSON, response-size
limits and exhausted host retries are intentional failures.
The client receives a safe 502 summary containing an opaque `error_id`, fixed
`vision_fallback_exhausted`, and ordered attempts with only `model`, `category`,
optional `upstream_status`, and `retryable`. Host executor errors remain generic;
provider text, credentials, full URLs/data URIs, and local paths are omitted.
For eligible image requests in any supported downstream protocol, plugin failures
are terminal; it never forwards the original image as a fallback.

## Docker issues

`docker build` requires a running daemon and access to the Go base image/module
proxy. Use `docker compose ... config` first to catch invalid paths or YAML.
Create the host directories referenced by the compose file and verify that the
plugin is under `plugins/linux/amd64`, not directly under `plugins/`; this path
is specific to the Linux amd64 container shown in the Compose example.

## Slow or rejected requests

Adjust `max_inflight_vision_requests` or increase the bounded total timeout only
after checking VLM latency. Prompt groups queue instead of being rejected when
all callback slots are occupied. A 413 response distinguishes request-body,
image-reference and emergency unique-image limits. Check the matching CLIProxyAPI warning
from `host.log` for `limit_kind`, `actual`, `maximum`, active size settings and
`config_generation`; ABI admission warnings include their byte budget and
in-flight usage. This ordinary host-log warning never contains image data or
credentials. Provider retry behavior is controlled by CLIProxyAPI.

If the target model nevertheless calls `view_image`, verify that the rewritten
prompt contains the fixed vision-preprocessing notice and no local attachment
path unless `agent_reanalysis_enabled: true` and the request explicitly
declared `view_image`. A successful rich `view_image` tool result is an array
and is converted only when the switch and declaration permit controlled
reanalysis; a string result (including a tool error) is preserved as a normal
Responses function output. The plugin trusts only actual image blocks in the
array, never paths or image-like tool arguments.

For `deepseek_vision_reanalyze`, check the exact argument contract: 1–16
opaque Agent-owned `attachment_ids` (validated but never resolved/read), required non-empty `focus` up to 2,000 characters, `detail`
`high`/`original` (default `high`), and `cache` `refresh`/`no_store` (default
`refresh`). At most three active tail call IDs are accepted. A new refresh call
ID runs once; an identical replay by call ID plus image/URL fingerprints,
focus/language/model chain is idempotent, while different identity for the same
ID is rejected. `no_store` leaves no cross-request cache entry. Setting
`analysis_cache_size: 0` affects only ordinary analysis caching, not the
separate bounded refresh idempotency cache. Strictly
validated `.codex/attachments/<id>/` paths survive only when `view_image` is
declared; all other local paths are redacted.

For a multi-turn request whose ordinary 413 warning is insufficient, enable:

```yaml
trace_enabled: true
```

Reproduce once, then inspect `logs/deepseek-vision-trace/events.jsonl` and the
referenced request bundle. `20-discovery.json` includes total, unique,
duplicate, prompt-group, last-image-item, earlier-item, content, and
function-output image counts. VLM artifacts show each multi-image call and any
413 split batches. The inbound body and image references are preserved in plaintext, so
disable tracing and remove the bundle after diagnosis.

## Opt-in live-provider test failures

The live test is intentionally outside CI because it is billable and depends on
remote model availability. Confirm `CLIPROXY_ROOT`, `DEEPSEEK_API_KEY`,
`GPT_API_KEY`, and the full OpenAI-compatible `GPT_BASE_URL` first. Query the
provider's model list separately when a configured model is unknown. A text
model that returns 200 for ordinary prompts may still reject image data; choose
a model that accepts Responses or Chat image input after CLIProxyAPI protocol
translation.

Use a comma-separated `VISION_FALLBACK_MODELS` list to exercise the real chain.
`LIVE_EXPECT_FALLBACK=1` makes the test require at least one failed attempt and
an `attempt-02` trace artifact; leave it off for an ordinary primary-success
run. HTTP 400 is intentionally non-retryable, while 408, 429, and 5xx may move
to the next configured model.

Do not rely on a remote model to keep returning an error: provider health and
routing can change between probes. `LIVE_FORCE_PRIMARY_STATUS=503` gives the
primary alias a deterministic private loopback failure while retaining a real
remote fallback. The private fixture returns the injected HTTP status, but
CLIProxyAPI's host callback does not expose that provider status to the plugin.
The observable trace contract is therefore a safe
`host_executor_error` with no `upstream_status`, followed by a selected attempt
2 for every VLM job. This is intentional: the plugin must not mislabel a
callback failure as a network, authentication, or provider HTTP error.

If `LIVE_USE_CODEX=1` fails after the deterministic phase passes, inspect the
sanitized `codex.log` and trace with `KEEP_LIVE_TMP=1`. The likely distinction is
that provider transport and plugin rewriting succeeded, but the text model did
not choose `view_image` or returned a tool shape Codex could not execute. Do not
promote that nondeterministic Codex behavior to a release gate; use the
deterministic two-request phase to diagnose the plugin contract.

When a thinking-mode text provider says `reasoning_content` was not returned,
verify that the client echoed the opaque Responses reasoning item before the
assistant function calls. The deterministic live script copies that item from
its successful first response without decoding or fabricating it. Real Codex
normally preserves the same item automatically in its tool loop.
