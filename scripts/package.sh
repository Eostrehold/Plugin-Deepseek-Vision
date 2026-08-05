#!/usr/bin/env bash
set -euo pipefail

# Build and package a native c-shared plugin target. Vision-model routing and
# credentials are host-owned; this script never reads or writes credentials.
repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
version="${VERSION:-0.1.1}"
dist_dir="${DIST_DIR:-${repo_dir}/dist}"
go_bin="${GO:-go}"
python_bin="${PYTHON:-python3}"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid VERSION: $version" >&2
  exit 2
fi
command -v "$go_bin" >/dev/null 2>&1 || { echo "Go toolchain not found: $go_bin" >&2; exit 2; }
if ! command -v "$python_bin" >/dev/null 2>&1; then
  python_bin=python
fi
command -v "$python_bin" >/dev/null 2>&1 || { echo "Python is required for deterministic ZIP packaging" >&2; exit 2; }
command -v git >/dev/null 2>&1 || { echo "git is required for source credential scanning" >&2; exit 2; }

host_goos="$($go_bin env GOOS)"
host_goarch="$($go_bin env GOARCH)"
target_goos="${TARGET_GOOS:-$host_goos}"
target_goarch="${TARGET_GOARCH:-$host_goarch}"
if [[ "$host_goos" != "$target_goos" || "$host_goarch" != "$target_goarch" ]]; then
  echo "package.sh requires a native target host (host ${host_goos}/${host_goarch}, target ${target_goos}/${target_goarch})" >&2
  exit 2
fi
case "${target_goos}/${target_goarch}" in
  linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|windows/arm64|freebsd/amd64) ;;
  *)
    echo "unsupported plugin package target: ${target_goos}/${target_goarch}" >&2
    exit 2
    ;;
esac
case "$target_goos" in
  darwin) extension=".dylib" ;;
  windows) extension=".dll" ;;
  *) extension=".so" ;;
esac

mkdir -p "$dist_dir"
build_dir="$(mktemp -d "${TMPDIR:-/tmp}/deepseek-vision-package.XXXXXX")"
trap 'rm -rf "$build_dir"' EXIT
artifact="$build_dir/deepseek-vision${extension}"
archive="$dist_dir/deepseek-vision_${version}_${target_goos}_${target_goarch}.zip"

echo "building ${archive} with $($go_bin version)"
(
  cd "$repo_dir"
  CGO_ENABLED=1 GOOS="$target_goos" GOARCH="$target_goarch" GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" \
    "$go_bin" build -buildmode=c-shared -trimpath -buildvcs=false \
    -ldflags="-s -w -buildid= -X main.pluginVersion=${version}" \
    -o "$artifact" .
)

[[ -s "$artifact" ]] || { echo "compiler produced an empty artifact" >&2; exit 1; }
symbols_file="$build_dir/exports.txt"
case "$target_goos" in
  darwin)
    command -v nm >/dev/null 2>&1 || { echo "nm is required to verify the Darwin ABI export" >&2; exit 2; }
    nm -gU "$artifact" >"$symbols_file"
    ;;
  windows)
    objdump_bin="${OBJDUMP:-objdump}"
    command -v "$objdump_bin" >/dev/null 2>&1 || { echo "$objdump_bin is required to verify the Windows ABI export" >&2; exit 2; }
    "$objdump_bin" -p "$artifact" >"$symbols_file"
    ;;
  *)
    command -v nm >/dev/null 2>&1 || { echo "nm is required to verify the ABI export" >&2; exit 2; }
    nm -D "$artifact" >"$symbols_file"
    ;;
esac
grep -Eq '[[:space:]]_?cliproxy_plugin_init$' "$symbols_file" || {
  echo "artifact is missing cliproxy_plugin_init" >&2
  exit 1
}
ARTIFACT="$artifact" VERSION="$version" "$python_bin" - <<'PY' || {
import os
from pathlib import Path

artifact = Path(os.environ["ARTIFACT"])
version = os.environ["VERSION"].encode("ascii")
if version not in artifact.read_bytes():
    raise SystemExit("requested version metadata is not embedded")
PY
  echo "artifact does not contain requested version metadata: $version" >&2
  exit 1
}

# Guard against accidental local captures or credentials in checked-in files.
if git -C "$repo_dir" grep -nEI \
  -e 'sk-[A-Za-z0-9]{12,}' -e 'Bearer[[:space:]]+[A-Za-z0-9._-]{16,}' -- . >/dev/null; then
  echo "possible credential material found in repository; refusing to package" >&2
  exit 1
fi

VERSION="$version" ARTIFACT="$artifact" ARCHIVE="$archive" LIBRARY_NAME="deepseek-vision${extension}" "$python_bin" - <<'PY'
import os
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile, ZipInfo

artifact = Path(os.environ["ARTIFACT"])
archive = Path(os.environ["ARCHIVE"])
version = os.environ["VERSION"]
library_name = os.environ["LIBRARY_NAME"]
payload = artifact.read_bytes()

archive.parent.mkdir(parents=True, exist_ok=True)
fixed_date = (2020, 1, 1, 0, 0, 0)
with ZipFile(archive, "w", compression=ZIP_DEFLATED, compresslevel=9) as zf:
    info = ZipInfo(library_name, date_time=fixed_date)
    info.compress_type = ZIP_DEFLATED
    info.create_system = 3
    info.external_attr = 0o755 << 16
    info.comment = f"deepseek-vision {version}".encode("ascii")
    zf.writestr(info, payload)
print(f"packaged {archive} ({archive.stat().st_size} bytes)")
PY

ARCHIVE="$archive" CHECKSUMS="${dist_dir}/checksums.txt" "$python_bin" - <<'PY'
import hashlib
import os
from pathlib import Path

archive = Path(os.environ["ARCHIVE"])
checksums = Path(os.environ["CHECKSUMS"])
digest = hashlib.sha256(archive.read_bytes()).hexdigest()
checksums.write_text(f"{digest}  {archive.name}\n", encoding="ascii")
print(f"wrote {checksums}")
PY
