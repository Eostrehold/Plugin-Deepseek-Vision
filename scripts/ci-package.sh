#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
target_goos="${TARGET_GOOS:?TARGET_GOOS is required}"
target_goarch="${TARGET_GOARCH:?TARGET_GOARCH is required}"
dist_dir="${DIST_DIR:-dist}"
version="${VERSION:-0.3.0}"

if [[ "$target_goos" != "linux" ]]; then
  VERSION="$version" TARGET_GOOS="$target_goos" TARGET_GOARCH="$target_goarch" \
    "$repo_dir/scripts/package-smoke.sh"
  VERSION="$version" TARGET_GOOS="$target_goos" TARGET_GOARCH="$target_goarch" DIST_DIR="$dist_dir" \
    "$repo_dir/scripts/package.sh"
  exit 0
fi

case "$target_goarch" in
  amd64) manylinux_image="quay.io/pypa/manylinux2014_x86_64" ;;
  arm64) manylinux_image="quay.io/pypa/manylinux2014_aarch64" ;;
  *) echo "unsupported manylinux target: linux/$target_goarch" >&2; exit 2 ;;
esac

command -v docker >/dev/null 2>&1 || { echo "Docker is required for the GLIBC 2.17 Linux build" >&2; exit 2; }

docker run --rm \
  -v "$repo_dir:/src" \
  -w /src \
  -e "GO_VERSION=${GO_VERSION:-1.26.4}" \
  -e "VERSION=$version" \
  -e "TARGET_GOOS=$target_goos" \
  -e "TARGET_GOARCH=$target_goarch" \
  -e "DIST_DIR=$dist_dir" \
  "$manylinux_image" \
  bash -euo pipefail -c '
    python_bin="$(command -v python3 || true)"
    if [[ -z "$python_bin" ]]; then
      for candidate in /opt/python/cp3*/bin/python3; do
        if [[ -x "$candidate" ]]; then
          python_bin="$candidate"
          break
        fi
      done
    fi
    [[ -n "$python_bin" ]] || { echo "Python 3 not found in manylinux image" >&2; exit 2; }
    export PYTHON="$python_bin"
    for tool in curl git nm readelf tar; do
      command -v "$tool" >/dev/null 2>&1 || { echo "$tool not found in manylinux image" >&2; exit 2; }
    done
    git config --global --add safe.directory /src
    go_archive="go${GO_VERSION}.linux-${TARGET_GOARCH}.tar.gz"
    curl -fsSL "https://go.dev/dl/${go_archive}" -o "/tmp/${go_archive}"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${go_archive}"
    export PATH="/usr/local/go/bin:${PATH}"
    ./scripts/package-smoke.sh
    ./scripts/package.sh

    archive="${DIST_DIR}/deepseek-vision_${VERSION}_${TARGET_GOOS}_${TARGET_GOARCH}.zip"
    "$PYTHON" - "$archive" /tmp/deepseek-vision.so <<"PY"
from pathlib import Path
from zipfile import ZipFile
import sys

with ZipFile(sys.argv[1]) as archive:
    Path(sys.argv[2]).write_bytes(archive.read("deepseek-vision.so"))
PY
    glibc_versions="$(readelf --version-info /tmp/deepseek-vision.so | sed -n "s/.*Name: GLIBC_\([0-9.]*\).*/\1/p" | sort -Vu)"
    if [[ -n "$glibc_versions" ]]; then
      printf "GLIBC versions:\n%s\n" "$glibc_versions"
      max_glibc="$(printf "%s\n" "$glibc_versions" | sort -V | tail -n 1)"
      if [[ "$(printf "2.17\n%s\n" "$max_glibc" | sort -V | tail -n 1)" != "2.17" ]]; then
        echo "plugin requires GLIBC_${max_glibc}, expected GLIBC_2.17 or older" >&2
        exit 1
      fi
    fi
  '
