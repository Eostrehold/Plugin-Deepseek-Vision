#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
go_bin="${GO:-go}"
python_bin="${PYTHON:-python3}"
if ! command -v "$python_bin" >/dev/null 2>&1; then
  python_bin=python
fi
command -v "$python_bin" >/dev/null 2>&1 || { echo "Python is required for package smoke verification" >&2; exit 2; }
target_goos="${TARGET_GOOS:-$($go_bin env GOOS)}"
target_goarch="${TARGET_GOARCH:-$($go_bin env GOARCH)}"
case "$target_goos" in
  darwin) extension=".dylib" ;;
  windows) extension=".dll" ;;
  *) extension=".so" ;;
esac
smoke_dir="$(mktemp -d "${TMPDIR:-/tmp}/deepseek-vision-smoke.XXXXXX")"
trap 'rm -rf "$smoke_dir"' EXIT

VERSION="${VERSION:-0.2.0}" DIST_DIR="$smoke_dir/dist" \
  "$repo_dir/scripts/package.sh"
VERSION="${VERSION:-0.2.0}" DIST_DIR="$smoke_dir/dist" \
  "$repo_dir/scripts/checksum.sh"

VERSION="${VERSION:-0.2.0}" DIST_DIR="$smoke_dir/dist" ARCHIVE_ROOT="$smoke_dir/unpacked" \
TARGET_GOOS="$target_goos" TARGET_GOARCH="$target_goarch" LIBRARY_NAME="deepseek-vision${extension}" \
  "$python_bin" - <<'PY'
import hashlib
import os
import re
from pathlib import Path
from zipfile import ZipFile

dist = Path(os.environ["DIST_DIR"])
version = os.environ["VERSION"]
target_goos = os.environ["TARGET_GOOS"]
target_goarch = os.environ["TARGET_GOARCH"]
library_name = os.environ["LIBRARY_NAME"]
archive = dist / f"deepseek-vision_{version}_{target_goos}_{target_goarch}.zip"
unpacked = Path(os.environ["ARCHIVE_ROOT"])
with ZipFile(archive) as zf:
    names = zf.namelist()
    if names != [library_name]:
        raise SystemExit(f"unexpected ZIP members: {names!r}")
    zf.extractall(unpacked)
library = unpacked / library_name
if library.stat().st_mode & 0o400 == 0:
    raise SystemExit("plugin artifact is not readable")
blob = archive.read_bytes()
line = (dist / "checksums.txt").read_text().strip()
want = hashlib.sha256(blob).hexdigest()
if line != f"{want}  {archive.name}":
    raise SystemExit("release archive checksum mismatch")
if re.search(rb"sk-[A-Za-z0-9]{12,}", blob) or re.search(rb"Bearer\s+[A-Za-z0-9._-]{16,}", blob):
    raise SystemExit("possible credential marker in archive")
print(f"smoke-verified {archive}")
PY
