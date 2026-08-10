# Changelog

All notable release changes are documented here. Versions follow Semantic
Versioning, and release tags use the `vX.Y.Z` form.

## [0.3.1] - 2026-08-10

### Fixed

- Accept valid string-form OpenAI Responses `input[].content` values instead of
  rejecting the complete request as malformed. String and array user text now
  produce the same visual focus for later image-bearing items and controlled
  `view_image` reanalysis.
- Preserve rich `function_call_output.output[]` image discovery when the same
  item also carries string content.

### Changed

- Centralized ordered user-context selection for Responses, Chat Completions,
  and Anthropic Messages while retaining each protocol's existing local-text,
  tool-result, rewrite, error, ABI, and configuration contracts.

## [0.3.0] - 2026-08-09

### Added

- Ordered vision fallback candidates through `vision_fallback_models` (up to
  three entries after the primary `vision_model`). Fallback attempts stay
  inside the host executor boundary and expose only safe model/category/status
  fields on a terminal 502.
- Opt-in controlled agent reanalysis through declared `view_image` and
  `deepseek_vision_reanalyze` rich tool outputs. Reanalysis accepts exactly
  1–16 `attachment_ids`, a required `focus` of at most 2,000 characters,
  `detail` `high`/`original` (default `high`), and `cache` `refresh`/`no_store`
  (default `refresh`).
- `attachment_ids` are opaque Agent-owned handles: the plugin validates them but
  never resolves or reads them as image sources; only real tool-output image
  blocks are trusted.
- Idempotent refresh handling for a new call ID and an identical replay, with
  a maximum of three active tail call IDs per request. `no_store` never writes
  cross-request cache state.
- An opt-in, billable live-provider E2E that deterministically reproduces a
  Codex-shaped rich tool-output round and can additionally run a genuine
  ephemeral `codex exec` loop.

### Changed

- Rich reanalysis trusts only actual image blocks in the corresponding tool
  output; tool arguments never supply image bytes or references.
- Active reanalysis is restricted to the terminal protocol-native tool-output
  suffix. `view_image` rejects malformed arguments, unsupported fields, and
  invalid detail values instead of silently downgrading them.
- The generation-local refresh idempotency cache enforces its capacity even
  while all entries are pending. Public errors and ordinary diagnostics redact
  URL-, path-, or credential-shaped configured model labels.
- Normal image paths remain redacted. When reanalysis is enabled, strictly
  validated paths under `.codex/attachments/<id>/` are retained only when the
  request explicitly declares `view_image`.
- 502 responses are structured with an opaque `error_id` and safe ordered
  `attempts`; host executor failures remain generic and provider response text,
  credentials, URLs, and local paths are not exposed.
- Existing YAML remains compatible. The plugin still reuses
  CLIProxyAPI's `host.model.execute`; it does not add or require a
  CLIProxyAPI server-side tool.
- Plaintext VLM trace artifacts include an attempt number, preserving every
  fallback model's request and response instead of overwriting earlier
  attempts within the same job.
- The Management form exposes `target_models` as a JSON array alongside the
  primary and fallback vision-model fields.

### Release boundary

- Validated plugin SDK baseline: CLIProxyAPI v7.2.119.
- Host process and dynamic-loader E2E: CLIProxyAPI v7.2.119 and v7.2.121.
- Published artifact targets: Linux, macOS, and Windows on amd64/arm64.
- Release-tested DeepSeek target: `deepseek-v4-flash`.

## [0.2.0] - 2026-08-05

### Added

- Image preprocessing for OpenAI Chat Completions and Anthropic Messages,
  including visible history and image-bearing tool results.
- Protocol-native fail-closed errors for Anthropic clients.

### Changed

- Generalized downstream request planning while keeping host-owned upstream
  protocol translation and vision execution unchanged.
- Updated the validated CLIProxyAPI SDK baseline to v7.2.119.
- Existing `target_models` now apply automatically to Responses, Chat
  Completions, and Anthropic Messages routes.

### Release boundary

- Validated host SDK: CLIProxyAPI v7.2.119.
- Published artifact targets remain Linux, macOS, and Windows on amd64/arm64.
- Release-tested DeepSeek target remains `deepseek-v4-flash`.
- Image file IDs, hidden server-side history, documents, and response-stream
  rewriting remain outside the supported conversion boundary.

## [0.1.1] - 2026-08-04

Initial public release.

### Added

- Native CLIProxyAPI v7 request interception for image-bearing OpenAI
  Responses requests targeting `deepseek-v4-flash`.
- Host-owned vision execution through `host.model.execute`, reusing existing
  model routing, credentials, protocol translation, transport, and retries.
- Ordered multi-image analysis per prompt item, including visible historical
  turns and array-form function-call outputs.
- Request-local deduplication and configurable process-local TTL caching of
  derived prompt-group analysis.
- Global in-flight vision backpressure, a high emergency unique-image ceiling,
  and adaptive ordered splitting only after an explicit host 413 response.
- Atomic fail-closed rewriting with a final guarantee that no `input_image`
  reaches the non-vision target model.
- A fixed downstream notice that tells DeepSeek to use the supplied visual
  analysis instead of reopening consumed attachments with `view_image`.
- Bilingual CPAMC configuration fields with defaults and validation ranges.
- Full-context opt-in trace bundles for multi-turn request diagnosis.
- Task-focused, low-reasoning vision requests that avoid exhaustive unrelated
  OCR while retaining automatic image detail and actionable visual evidence.
- Deterministic Linux, macOS, and Windows amd64/arm64 packaging, aggregate
  checksums, contract validation, race tests, mock-host E2E coverage, and
  manually triggered Draft Release generation.

### Release boundary

- Validated host SDK: CLIProxyAPI v7.2.113.
- Published artifact targets: Linux, macOS, and Windows on amd64/arm64.
- Release-tested DeepSeek target: `deepseek-v4-flash`.
- Chat Completions, Anthropic Messages, `file_id`-only images, and hidden
  `previous_response_id` history are outside the supported conversion boundary.

[0.3.1]: https://github.com/Zesuy/Plugin-Deepseek-Vision/releases/tag/v0.3.1
[0.3.0]: https://github.com/Zesuy/Plugin-Deepseek-Vision/releases/tag/v0.3.0
[0.2.0]: https://github.com/Zesuy/Plugin-Deepseek-Vision/releases/tag/v0.2.0
[0.1.1]: https://github.com/Zesuy/Plugin-Deepseek-Vision/releases/tag/v0.1.1
