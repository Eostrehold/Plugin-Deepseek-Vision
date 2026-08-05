#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
dist_dir="${DIST_DIR:-${repo_dir}/dist}"
version="${VERSION:-0.1.1}"
go_bin="${GO:-go}"
python_bin="${PYTHON:-python3}"
if ! command -v "$python_bin" >/dev/null 2>&1; then
  python_bin=python
fi
command -v "$go_bin" >/dev/null 2>&1 || { echo "Go toolchain not found: $go_bin" >&2; exit 2; }
command -v "$python_bin" >/dev/null 2>&1 || { echo "Python is required for checksum verification" >&2; exit 2; }
target_goos="${TARGET_GOOS:-$($go_bin env GOOS)}"
target_goarch="${TARGET_GOARCH:-$($go_bin env GOARCH)}"
archive="${dist_dir}/deepseek-vision_${version}_${target_goos}_${target_goarch}.zip"
checksums="${dist_dir}/checksums.txt"

[[ -f "$archive" ]] || { echo "archive not found: $archive" >&2; exit 1; }
ARCHIVE="$archive" CHECKSUMS="$checksums" "$python_bin" - <<'PY'
import hashlib
import os
from pathlib import Path

archive = Path(os.environ["ARCHIVE"])
checksums = Path(os.environ["CHECKSUMS"])
actual = f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}"
if not checksums.exists():
    checksums.write_text(actual + "\n", encoding="ascii")
else:
    expected = next((line for line in checksums.read_text(encoding="ascii").splitlines() if line.split(maxsplit=1)[-1] == archive.name), "")
    if not expected:
        raise SystemExit(f"no checksum entry for {archive.name}")
    if expected != actual:
        raise SystemExit(f"checksum mismatch for {archive.name}\nexpected: {expected}\nactual:   {actual}")
print(actual)
PY
