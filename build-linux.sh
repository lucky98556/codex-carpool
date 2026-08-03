#!/usr/bin/env bash
# Linux amd64 plugin verification and packaging entrypoint.
# Usage: ./build-linux.sh
# Override package version: VERSION=1.0.0 ./build-linux.sh

set -Eeuo pipefail

project_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
package_version="${VERSION:-1.0.0}"

cd "$project_dir"

for command_name in go make zip sha256sum; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "Missing required command: $command_name" >&2
    exit 1
  fi
done

if [[ "$(go env GOOS)" != "linux" || "$(go env GOARCH)" != "amd64" ]]; then
  echo "This script must run on Linux amd64. Current target: $(go env GOOS)/$(go env GOARCH)" >&2
  exit 1
fi

echo '==> Resolve and verify dependencies'
go mod tidy
go mod verify

echo '==> Unit tests'
make test

echo '==> Race tests'
make test-race

echo '==> Static checks'
go vet ./...

echo "==> Build and package version ${package_version}"
make package VERSION="$package_version"

plugin_file="dist/codex-carpool_${package_version}.so"
archive_file="dist/codex-carpool_${package_version}_linux_amd64.zip"
test -s "$plugin_file"
test -s "$archive_file"

sha256sum "$plugin_file" "$archive_file" | tee "${archive_file}.sha256"

echo
echo '==> Build succeeded'
ls -lh "$plugin_file" "$archive_file" "${archive_file}.sha256"
