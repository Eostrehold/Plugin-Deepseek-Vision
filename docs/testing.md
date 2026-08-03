# Testing and release verification

Run the local checks from the repository root:

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/verify-contracts.sh
./scripts/package-smoke.sh
```

The package smoke test builds with CGO, verifies `cliproxy_plugin_init`, embeds
and checks the plugin SHA-256, checks ZIP root members and scans the archive for
obvious credential material. It uses a temporary directory and removes it on
exit. The regular package command writes only to the ignored `dist/` directory.

For Docker validation, first check the rendered configuration without starting
services:

```bash
docker compose -f docker/docker-compose.example.yml config
docker build --file Dockerfile.plugin --target artifact \
  --build-arg VERSION=0.1.0 \
  --output type=local,dest=/tmp/deepseek-vision-plugin .
```

End-to-end validation should use a mock VLM and mock DeepSeek upstream to assert
that a failed VLM call results in zero downstream calls. A real VLM check may
use `gpt-5.6-luna` with a development-only key supplied through the shell; do
not place that key in fixtures, Docker build arguments, CI or committed files.
The real gateway check and release acceptance target `deepseek-v4-flash` only.
`deepseek-v4-pro` remains a future-supported configuration target and is not
required or probed while its Responses endpoint is unavailable.
