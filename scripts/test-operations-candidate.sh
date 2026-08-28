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

declare -a command
case "${1:-}" in
verify)
  command=("$BINARY" verify --root "$ROOT" "${@:2}")
  ;;
verify-go-events | redact-stream)
  command=("$BINARY" "$@")
  ;;
run)
  command=("$BINARY" run --root "$ROOT" "${@:2}")
  ;;
*)
  command=("$BINARY" --root "$ROOT" "$@")
  ;;
esac

set +e
"${command[@]}"
status=$?
set -e
exit "$status"
