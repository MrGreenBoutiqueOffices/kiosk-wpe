#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT

# Internal controller tests must compile in the same package as the source.
# Stage them together without exporting production internals or copying code into Git.
cp "$repo_dir/go.mod" "$repo_dir/kiosk_controller.go" "$test_dir/"
cp "$repo_dir"/tests/*_test.go "$test_dir/"
cd "$test_dir"

if [ "${1:-}" = "--pid1" ]; then
    shift
    CGO_ENABLED=0 GOOS=linux go test -c -o "$test_dir/kiosk-tests" .
    docker run --rm --network none \
        --mount "type=bind,src=$test_dir/kiosk-tests,dst=/kiosk-tests,readonly" \
        --entrypoint /kiosk-tests debian:trixie-slim -test.timeout 120s "$@"
else
    go test "$@" ./...
fi
