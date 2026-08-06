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
  --build-arg VERSION=0.2.0 \
  --output type=local,dest=/tmp/deepseek-vision-plugin .
```

End-to-end validation uses a real CLIProxyAPI process with mock providers to
exercise Responses, Chat Completions, and Anthropic Messages routes. It asserts
that `host.model.execute` performs routing, credentials, protocol translation,
and self-skip without recursion; rewritten upstream requests contain no image
blocks; and a failed VLM call results in zero business-upstream calls. No plugin
key is required. Release acceptance targets `deepseek-v4-flash`;
`deepseek-v4-pro` remains future-supported only.
