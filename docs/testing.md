# Testing and release verification

Run the local checks from the repository root:

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/verify-contracts.sh
./scripts/package-smoke.sh
```

The package smoke test builds with CGO, verifies `cliproxy_plugin_init`, checks
the external archive SHA-256, requires the ZIP root to contain only the native
`deepseek-vision.so`, `.dylib`, or `.dll`, and scans the archive for obvious
credential material. Ordinary CI runs the Linux amd64 package in the same
manylinux2014 environment used by CLIProxyAPI. The Release workflow performs
the native Linux, macOS, and Windows amd64/arm64 matrix. Smoke tests use a
temporary directory and remove it on exit; the regular package command writes
only to the ignored `dist/` directory.
The Windows arm64 job installs a pinned, SHA-256-verified LLVM-MinGW toolchain
because the hosted runner's default `gcc` targets x86-64.

For Docker validation, first check the rendered configuration without starting
services:

```bash
docker compose -f docker/docker-compose.example.yml config
docker build --file docker/Dockerfile.plugin --target artifact \
  --build-arg VERSION=0.3.1 \
  --output type=local,dest=/tmp/deepseek-vision-plugin .
```

End-to-end validation uses a real CLIProxyAPI process with mock providers to
exercise Responses, Chat Completions, and Anthropic Messages routes. It asserts
that `host.model.execute` performs routing, credentials, protocol translation,
and self-skip without recursion; rewritten upstream requests contain no image
blocks; and a failed VLM call results in zero business-upstream calls. No plugin
key is required. Release acceptance targets `deepseek-v4-flash`;
`deepseek-v4-pro` remains future-supported only.

The host E2E also enables the opt-in plaintext trace inside its disposable
private directory. For the Codex-shaped rich tool-output case it verifies the
inbound body, discovery and cache plan, every VLM request/response/result,
rewritten body, event correlation, file permissions, focus text, and removal of
image blocks from the business-upstream request. Fallback attempts have distinct
`attempt-NN` artifact names so a later model cannot overwrite an earlier
model's request or response.

v0.3.1 acceptance also covers the ordered primary-plus-fallback chain: retryable
HTTP 408/429/5xx, attempt timeouts, invalid/empty/oversized results, and generic
host executor errors advance in order, while parent cancellation and rewrite
failures do not. Assertions must verify that terminal 502 bodies contain only
`error_id`, `vision_fallback_exhausted`, and safe attempt fields (`model`,
`category`, optional `upstream_status`, `retryable`). Provider response text,
credentials, complete image references, and local paths must be absent.

When `agent_reanalysis_enabled` is true, integration fixtures declare
`view_image` or `deepseek_vision_reanalyze` and place actual image blocks in the
matching rich tool output. Test 1–16 opaque Agent-owned `attachment_ids`
(validated but never resolved/read), required focus at the 2,000-character
boundary, detail defaults/validation, refresh/no-store cache semantics,
identical refresh replay idempotency by call ID plus image/URL
fingerprints/focus/language/model chain, conflicting call IDs, and the
three-active-tail-call limit. Verify that arguments alone cannot create an
image, `no_store` leaves no cross-request entry, `analysis_cache_size: 0` does
not disable the separate bounded refresh idempotency cache, and only declared
`.codex/attachments/<id>/` paths survive sanitization. These tests require no
real provider key or local user path.

## Opt-in live-provider and Codex test

`scripts/live-provider-e2e.sh` is a billable, non-CI test for a real DeepSeek
text provider and a real OpenAI-compatible vision provider. Its deterministic
phase builds a temporary CLIProxyAPI process, sends two Responses requests that
reproduce a Codex `view_image` plus `deepseek_vision_reanalyze` rich-output
round, and validates the resulting plaintext trace. The second request keeps
its ordinary user focus in the string `content` shape so this live gate covers
the Issue #8 regression, not only the equivalent `input_text` array shape. It
generates a small local PNG fixture and does not use a repository or user
attachment. If the text
provider returns an opaque Responses reasoning item, the synthetic second round
echoes it before the assistant function calls exactly as a real Codex tool loop
does; this keeps thinking-mode provider history valid without inspecting or
inventing reasoning content.

```bash
CLIPROXY_ROOT=/path/to/CLIProxyAPI-v7.2.121 \
DEEPSEEK_API_KEY=... \
GPT_API_KEY=... \
GPT_BASE_URL=https://vision.example.test/v1 \
./scripts/live-provider-e2e.sh
```

Defaults are `DEEPSEEK_BASE_URL=https://api.deepseek.com/v1`,
`DEEPSEEK_MODEL=deepseek-v4-flash`, and
`VISION_MODEL=glm-4.6v-flash`. Override those names for the models exposed by
the selected providers. The vision provider uses standard Bearer authentication
by default. Set `GPT_API_KEY_HEADER=x-api-key` for an OpenAI-compatible endpoint
that authenticates with `x-api-key`; the disposable CLIProxyAPI configuration
then adds that provider header. A comma-separated `VISION_FALLBACK_MODELS`
configures the real ordered chain. Set `LIVE_EXPECT_FALLBACK=1` only when the
selected primary is expected to fail with a retryable condition and the trace
must prove that a later attempt was selected.

For a deterministic hybrid fallback test, also set
`LIVE_FORCE_PRIMARY_STATUS=503`. The script then routes only the synthetic
primary attempt to a private loopback fixture returning that retryable status;
the configured fallback still runs against the real vision provider. This is
preferable to depending on a remote model to remain broken. Allowed injected
statuses are 408, 429, and 5xx, and the mode requires both
`LIVE_EXPECT_FALLBACK=1` and at least one `VISION_FALLBACK_MODELS` entry.
CLIProxyAPI intentionally collapses the failed provider execution at the host
callback boundary, so the plugin trace must record safe
`host_executor_error` with no invented upstream status, followed by a selected
attempt 2. The fixture status tests the retryable host path, not provider-status
passthrough.

Set `LIVE_USE_CODEX=1` to add a genuine ephemeral `codex exec -i` run through
the temporary local host. This extra layer requires the target text model to
obey the requested `view_image` tool call, so it is deliberately not a stable
release or CI gate. It also requires `timeout` and `setsid`; the latter isolates
Codex and any background marketplace children so cleanup can terminate only
that test process group. The deterministic Codex-shaped phase remains
authoritative for the protocol contract.

The script writes provider keys only to a mode-0600 temporary CLIProxyAPI
configuration, starts the host without those keys in its process environment,
and gives Codex only a local temporary client key. The credential-bearing
configuration is removed on exit. By default every other artifact is deleted;
`KEEP_LIVE_TMP=1` preserves sanitized logs and trace data for debugging. Even
after sanitization, the trace still contains the complete image and
conversation, so treat it as high sensitivity and remove it when finished. The
preserve path is fail-closed: if redaction or the final whole-directory exact
key scan cannot complete, the script deletes the temporary directory and exits
non-zero instead of labelling it sanitized.
