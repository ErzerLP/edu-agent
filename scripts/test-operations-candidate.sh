#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
BINARY=$(mktemp "${TMPDIR:-/tmp}/edu-agent-operations-candidate.XXXXXX")
cleanup() {
  local status=$?
  trap - EXIT HUP INT TERM
  rm -f "$BINARY"
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

cd "$ROOT/contracttests/operations"
go build -o "$BINARY" ./cmd/operations-candidate
set +e
"$BINARY" --root "$ROOT" "$@"
status=$?
set -e
exit "$status"
