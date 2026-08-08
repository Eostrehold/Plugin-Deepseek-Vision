# Security notes

## Credentials and data

- The plugin reuses the ordered models and credentials already configured in CLIProxyAPI;
  it has no key field, environment-variable credential, or external backend.
- Ordinary host diagnostics do not log Authorization headers, complete endpoint
  URLs, image data URIs, or upstream response bodies. The explicitly enabled
  plaintext trace has the broader data boundary documented below.
- The plugin caches only derived analysis text and SHA-256 keys. It never keeps
  raw image bytes or the original image reference in cache entries; URL entries
  use a shorter TTL because their content may change. Reanalysis `no_store`
  never reads or writes cross-request cache state; `refresh` is idempotent only
  for an identity made from call ID, decoded-image or normalized URL fingerprints,
  focus, normalized language, and the full ordered model chain (detail affects
  the returned image fingerprint; cache mode is not a separate identity field).
  `analysis_cache_size: 0` disables only the ordinary analysis LRU; the separate
  bounded generation-local call-ID idempotency cache remains available.
- Package scripts scan source and generated archives for obvious key markers and
  fail closed. This is a guardrail, not a replacement for organization-wide
  secret scanning.

## Network

CLIProxyAPI owns provider transport, retries, concurrency, and credential
handling. Literal loopback/private/link-local image
URLs are rejected; deployment DNS names still need an egress/allowlist policy.
Prefer HTTPS for remote image references.

The plugin sends one prompt item's ordered image references and bounded text
context to each fallback candidate in order. CLIProxyAPI chooses the provider
for every configured model. Review retention,
access control and data residency of the chosen provider.

## Prompt injection and failure safety

Text visible in an image is untrusted data. The prompt explicitly asks the VLM
not to follow instructions found inside the image. If any image analysis fails,
the complete request is terminated before the DeepSeek executor receives it;
partial rewrites and accidental original-image forwarding are not allowed.
For successfully consumed Codex attachments, matching local temporary paths are
removed by default. The only opt-in exception requires
`agent_reanalysis_enabled: true`, an explicitly declared `view_image`, and a
strictly validated `.codex/attachments/<id>/` path; broader local paths remain
redacted. A fixed plugin-authored notice tells the non-vision target model to
use supplied analysis rather than invoke image/file tools on redacted paths.

Agent reanalysis trusts only actual image blocks in the matching rich tool
output. `attachment_ids` are opaque Agent-owned handles: the plugin validates
1–16 non-empty strings but never resolves or reads them, and never treats them
as image data or a path. `deepseek_vision_reanalyze` requires
a non-empty focus of at most 2,000 characters, detail `high`/`original` (default
`high`), and cache `refresh`/`no_store` (default `refresh`). Only three active
tail call IDs are allowed per request.

`max_inflight_vision_requests` bounds callback fan-out without rejecting normal
multi-image prompts. `emergency_max_images_per_request` is a high, last-resort
unique-image abuse boundary; body/reference limits remain independent byte
boundaries. Provider retry and rate limiting remain host-owned.

Configured-limit and ABI-admission rejections emit a structured warning through
the host logger. Diagnostics are intentionally restricted to limit names,
integer sizes/counts, active limit values and configuration generation; image
references, request bodies, headers and credentials are never included.

Terminal 502 responses contain only an opaque `error_id`, fixed
`vision_fallback_exhausted`, and ordered attempt summaries (`model`, safe
`category`, optional `upstream_status`, `retryable`). Host executor failures
remain the generic `host_executor_error`; provider response text, credentials,
complete image references, and local paths are never returned. Fallbacks are
limited to retryable HTTP 408/429/5xx, attempt timeouts, invalid/empty/oversized
results, or generic host executor errors; parent cancellation, rewrite failure,
and other non-retryable conditions stop processing.

## Opt-in plaintext trace

`trace_enabled` deliberately changes the privacy boundary for debugging. It
writes complete conversation bodies, image URLs/data URIs, prompt-group context, VLM
requests/responses, and rewritten bodies beneath
`logs/deepseek-vision-trace/`. Credential-like header and metadata fields are
still forcibly redacted, but image URLs may themselves contain signed query
parameters. Protect the mounted logs directory, enable the switch only for a
bounded reproduction, and securely remove retained bundles afterward.

The opt-in `scripts/live-provider-e2e.sh` follows a narrower credential
boundary than the trace payload boundary. Provider keys are read from the
environment, written only to a mode-0600 configuration in a private temporary
directory, removed from the CLIProxyAPI and Codex process environments, and
deleted on exit. A genuine optional Codex run receives only the temporary local
CLIProxyAPI client key. `KEEP_LIVE_TMP=1` removes the credential-bearing config
and scrubs known keys from retained text artifacts, then scans every retained
file for the exact provider keys. A redaction/read/scan failure deletes the
whole temporary directory and fails the run; it is never reported as sanitized.
Successful sanitization still does not make the plaintext image/conversation
trace non-sensitive.
