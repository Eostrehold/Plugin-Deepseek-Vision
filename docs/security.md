# Security notes

## Credentials and data

- Store the VLM key in `DEEPSEEK_VISION_API_KEY` (or the configured environment
  variable), never in YAML, source, Dockerfiles, ZIP files or CI variables.
- The plugin does not log Authorization headers, complete endpoint URLs,
  image data URIs or upstream response bodies.
- Cache entries contain only visual-analysis text and metadata; raw image bytes
  are not retained.
- Package scripts scan source and generated archives for obvious key markers and
  fail closed. This is a guardrail, not a replacement for organization-wide
  secret scanning.

## Network

The VLM client uses an owned HTTP client with a total timeout, TLS handshake and
response-header deadlines, redirects disabled, response-size limits and finite
retry/backoff. Literal loopback/private/link-local image URLs are rejected;
deployment DNS names still need an egress/allowlist policy. Prefer HTTPS for
non-local endpoints.

The plugin sends the image reference and a bounded focus hint to the configured
VLM. Do not point `vision_base_url` at an untrusted service. Review retention,
access control and data residency of the chosen VLM provider.

## Prompt injection and failure safety

Text visible in an image is untrusted data. The prompt explicitly asks the VLM
not to follow instructions found inside the image. If any image analysis fails,
the complete request is terminated before the DeepSeek executor receives it;
partial rewrites and accidental original-image forwarding are not allowed.

Keep `max_images_per_request`, body/reference limits and `max_concurrency`
small enough for the deployment. These are both resource controls and abuse
boundaries. Rotate the key after development or if it may have appeared in a
shell history, process listing or external logs.
