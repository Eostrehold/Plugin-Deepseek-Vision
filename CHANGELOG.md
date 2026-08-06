# Changelog

All notable release changes are documented here. Versions follow Semantic
Versioning, and release tags use the `vX.Y.Z` form.

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

[0.2.0]: https://github.com/Zesuy/Plugin-Deepseek-Vision/releases/tag/v0.2.0
[0.1.1]: https://github.com/Zesuy/Plugin-Deepseek-Vision/releases/tag/v0.1.1
