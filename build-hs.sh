#!/usr/bin/env bash
# Build the static hs binary into the repo root, where boxes get it with
# `git pull` (they have no Go toolchain). Run on a dev machine, then commit
# the binary together with the source it was built from.
#
#   ./build-hs.sh          build + run the tests first
#   ./build-hs.sh --fast   build only
set -euo pipefail
cd "$(dirname "$(realpath "$0")")"

if [ "${1:-}" != "--fast" ]; then
  go vet ./internal/... ./cmd/...
  go test ./internal/...
fi

# The version is the commit the binary is built from; "-dirty" means it
# contains uncommitted source — commit first, then build, then commit hs.
version=$(git describe --always --dirty)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -X main.version=$version" -o hs ./cmd/hs
echo "built ./hs $version ($(du -h hs | cut -f1))"
case "$version" in *-dirty) echo "note: built from uncommitted source — commit the source, rebuild, then commit hs" ;; esac
