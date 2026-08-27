#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
SERVER_DIR="$ROOT/server"
POSTGRES_IMAGE=${POSTGRES_IMAGE:-postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73}
POSTGRES_TEST_TIMEOUT=${POSTGRES_TEST_TIMEOUT:-45m}
POSTGRES_TMPFS_SIZE=${POSTGRES_TMPFS_SIZE:-2g}
LOCK_FILE=${POSTGRES_CANDIDATE_LOCK_FILE:-${TMPDIR:-/tmp}/edu-agent-postgres-candidate.lock}
ALL_SHARDS=(db-core learning-core learning-offline learning-fault memory privacy-core privacy-fault)
SELECTED_SHARDS=()
RESUME=0
LIST_ONLY=0
EVIDENCE_DIR=${POSTGRES_EVIDENCE_DIR:-}
TEST_RUN_REGEX=${POSTGRES_TEST_RUN:-}
CONTAINER_NAME=""

usage() {
  cat <<'USAGE'
usage: scripts/test-postgres-candidate.sh [options]

Run the real PostgreSQL candidate matrix as stable, strictly serial shards.

Options:
  --shard NAME       Run one shard; repeat to select multiple shards.
  --resume           Skip shards with a matching passing evidence key.
  --evidence-dir DIR Store logs and pass markers in DIR.
  --run REGEX        Override the selected shard's Go test regex.
  --list              List shard names and exit.
  -h, --help          Show this help.

Environment:
  POSTGRES_IMAGE                Fixed digest-pinned PostgreSQL image.
  POSTGRES_TEST_TIMEOUT         Per-shard Go timeout (default: 45m).
  POSTGRES_TMPFS_SIZE           PostgreSQL tmpfs size (default: 2g).
  POSTGRES_EVIDENCE_DIR         Default evidence directory.
  POSTGRES_TEST_RUN             Default Go test regex override.
  POSTGRES_CANDIDATE_LOCK_FILE  Host-wide serialization lock.
  CANDIDATE_ID                  Required for source archives without .git metadata.
USAGE
}

contains_shard() {
  local wanted=$1
  local shard
  for shard in "${ALL_SHARDS[@]}"; do
    [[ "$shard" == "$wanted" ]] && return 0
  done
  return 1
}

while (($#)); do
  case "$1" in
  --shard)
    (($# >= 2)) || {
      echo "--shard requires a value" >&2
      exit 2
    }
    contains_shard "$2" || {
      echo "unknown shard: $2" >&2
      exit 2
    }
    SELECTED_SHARDS+=("$2")
    shift 2
    ;;
  --resume)
    RESUME=1
    shift
    ;;
  --evidence-dir)
    (($# >= 2)) || {
      echo "--evidence-dir requires a value" >&2
      exit 2
    }
    EVIDENCE_DIR=$2
    shift 2
    ;;
  --run)
    (($# >= 2)) || {
      echo "--run requires a value" >&2
      exit 2
    }
    TEST_RUN_REGEX=$2
    shift 2
    ;;
  --list)
    LIST_ONLY=1
    shift
    ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    echo "unknown argument: $1" >&2
    usage >&2
    exit 2
    ;;
  esac
done

if ((LIST_ONLY)); then
  printf '%s\n' "${ALL_SHARDS[@]}"
  exit 0
fi
if ((${#SELECTED_SHARDS[@]} == 0)); then
  SELECTED_SHARDS=("${ALL_SHARDS[@]}")
fi

for command in docker flock go git sha256sum; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 2
  }
done
[[ -d "$SERVER_DIR" ]] || {
  echo "server directory not found: $SERVER_DIR" >&2
  exit 2
}
[[ "$POSTGRES_IMAGE" == *@sha256:* ]] || {
  echo "POSTGRES_IMAGE must be pinned by digest" >&2
  exit 2
}

candidate_fingerprint() {
  (
    cd "$ROOT"
    {
      git rev-parse HEAD
      git diff --binary --no-ext-diff -- server
      while IFS= read -r path; do
        printf '%s\0' "$path"
        sha256sum "$path"
      done < <(git ls-files --others --exclude-standard -- server | LC_ALL=C sort)
    } | sha256sum | awk '{print $1}'
  )
}

CANDIDATE_FINGERPRINT=""
if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  CANDIDATE_FINGERPRINT=$(candidate_fingerprint)
elif [[ -n ${CANDIDATE_ID:-} ]]; then
  CANDIDATE_FINGERPRINT=$(printf '%s' "$CANDIDATE_ID" | sha256sum | awk '{print $1}')
else
  echo "CANDIDATE_ID is required when the candidate archive has no .git metadata" >&2
  exit 2
fi
RUNNER_SHA256=$(sha256sum "${BASH_SOURCE[0]}" | awk '{print $1}')
GO_VERSION=$(go version)
if [[ -z "$EVIDENCE_DIR" ]]; then
  EVIDENCE_DIR="${TMPDIR:-/tmp}/edu-agent-postgres-evidence/${CANDIDATE_ID:-$CANDIDATE_FINGERPRINT}"
fi
mkdir -p "$EVIDENCE_DIR"

shard_evidence_key() {
  local shard=$1
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
    "$CANDIDATE_FINGERPRINT" "$RUNNER_SHA256" "$POSTGRES_IMAGE" "$GO_VERSION" "$shard" "$TEST_RUN_REGEX" |
    sha256sum | awk '{print $1}'
}

printf 'candidate_fingerprint=%s\n' "$CANDIDATE_FINGERPRINT"
printf 'runner_sha256=%s\n' "$RUNNER_SHA256"
printf 'postgres_image=%s\n' "$POSTGRES_IMAGE"
printf 'go_version=%s\n' "$GO_VERSION"
printf 'evidence_dir=%s\n' "$EVIDENCE_DIR"

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  echo "another PostgreSQL candidate runner holds $LOCK_FILE" >&2
  exit 2
fi

PENDING_SHARDS=()
for shard in "${SELECTED_SHARDS[@]}"; do
  evidence_key=$(shard_evidence_key "$shard")
  pass_file="$EVIDENCE_DIR/$shard.$evidence_key.pass"
  if ((RESUME)) && [[ -f "$pass_file" ]]; then
    printf 'SKIP shard=%s reason=matching-pass-evidence key=%s\n' "$shard" "$evidence_key"
  else
    PENDING_SHARDS+=("$shard")
  fi
done
SELECTED_SHARDS=("${PENDING_SHARDS[@]}")
if ((${#SELECTED_SHARDS[@]} == 0)); then
  printf 'COMPLETE shards=all-selected-evidence-reused\n'
  exit 0
fi

cleanup() {
  local status=$?
  trap - EXIT HUP INT TERM
  if [[ -n "$CONTAINER_NAME" ]]; then
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

CONTAINER_NAME="edu-agent-postgres-candidate-$$"
docker run -d \
  --name "$CONTAINER_NAME" \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=edu_agent_test \
  -p 127.0.0.1::5432 \
  --tmpfs "/var/lib/postgresql/data:rw,noexec,nosuid,size=$POSTGRES_TMPFS_SIZE" \
  "$POSTGRES_IMAGE" >/dev/null

ready=0
for _ in $(seq 1 90); do
  if docker exec "$CONTAINER_NAME" pg_isready -U postgres -d edu_agent_test >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
((ready == 1)) || {
  echo "PostgreSQL did not become ready" >&2
  exit 1
}

HOST_BINDING=$(docker port "$CONTAINER_NAME" 5432/tcp | head -n 1)
HOST_PORT=${HOST_BINDING##*:}
[[ "$HOST_PORT" =~ ^[0-9]+$ ]] || {
  echo "unable to determine PostgreSQL host port" >&2
  exit 1
}
export TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:$HOST_PORT/edu_agent_test?sslmode=disable"

list_test_regex() {
  local package=$1
  local mode=$2
  local -a names=()
  local name
  while IFS= read -r name; do
    [[ "$name" =~ ^Test[[:alnum:]_]+$ ]] || continue
    case "$mode" in
    learning-core)
      [[ "$name" == TestPostgreSQLOffline* ]] && continue
      [[ "$name" == TestPostgreSQLTypedRecordWriteFaultMatrixRollsBack ]] && continue
      ;;
    learning-offline)
      [[ "$name" == TestPostgreSQLOffline* ]] || continue
      ;;
    privacy-core)
      [[ "$name" == TestPostgreSQLLocalPrivacyScrubFaultMatrix ]] && continue
      ;;
    *)
      echo "unsupported dynamic test mode: $mode" >&2
      return 2
      ;;
    esac
    names+=("$name")
  done < <(cd "$SERVER_DIR" && go test -list '^Test' "$package")
  ((${#names[@]} > 0)) || {
    echo "no tests selected for $mode" >&2
    return 1
  }
  local joined
  printf -v joined '%s|' "${names[@]}"
  joined=${joined%|}
  printf '^(%s)$' "$joined"
}

run_shard_command() {
  local shard=$1
  case "$shard" in
  db-core)
    local db_core_args=()
    [[ -z "$TEST_RUN_REGEX" ]] || db_core_args=(-run "$TEST_RUN_REGEX")
    (
      cd "$SERVER_DIR"
      go test -p=1 -count=1 -timeout="$POSTGRES_TEST_TIMEOUT" "${db_core_args[@]}" \
        ./internal/app \
        ./internal/knowledge/postgresstore \
        ./internal/platform/outbox/postgresstore \
        ./internal/platform/postgres \
        ./migrations
    )
    ;;
  learning-core)
    local learning_core_regex
    learning_core_regex=${TEST_RUN_REGEX:-$(list_test_regex ./internal/learning/postgresstore learning-core)}
    (cd "$SERVER_DIR" && go test -p=1 -count=1 -timeout="$POSTGRES_TEST_TIMEOUT" \
      -run "$learning_core_regex" ./internal/learning/postgresstore)
    ;;
  learning-offline)
    local learning_offline_regex
    learning_offline_regex=${TEST_RUN_REGEX:-$(list_test_regex ./internal/learning/postgresstore learning-offline)}
    (cd "$SERVER_DIR" && go test -p=1 -count=1 -timeout="$POSTGRES_TEST_TIMEOUT" \
      -run "$learning_offline_regex" ./internal/learning/postgresstore)
    ;;
  learning-fault)
    local learning_fault_regex=${TEST_RUN_REGEX:-^TestPostgreSQLTypedRecordWriteFaultMatrixRollsBack$}
    (cd "$SERVER_DIR" && go test -p=1 -count=1 -timeout="$POSTGRES_TEST_TIMEOUT" \
      -run "$learning_fault_regex" ./internal/learning/postgresstore)
    ;;
  memory)
    local memory_args=()
    [[ -z "$TEST_RUN_REGEX" ]] || memory_args=(-run "$TEST_RUN_REGEX")
    (cd "$SERVER_DIR" && go test -p=1 -count=1 -timeout="$POSTGRES_TEST_TIMEOUT" \
      "${memory_args[@]}" ./internal/memory/postgresstore)
    ;;
  privacy-core)
    local privacy_core_regex
    privacy_core_regex=${TEST_RUN_REGEX:-$(list_test_regex ./internal/privacy/postgresstore privacy-core)}
    (cd "$SERVER_DIR" && go test -p=1 -count=1 -timeout="$POSTGRES_TEST_TIMEOUT" \
      -run "$privacy_core_regex" ./internal/privacy/postgresstore)
    ;;
  privacy-fault)
    local privacy_fault_regex=${TEST_RUN_REGEX:-^TestPostgreSQLLocalPrivacyScrubFaultMatrix$}
    (cd "$SERVER_DIR" && go test -p=1 -count=1 -timeout="$POSTGRES_TEST_TIMEOUT" \
      -run "$privacy_fault_regex" ./internal/privacy/postgresstore)
    ;;
  *)
    echo "unsupported shard: $shard" >&2
    return 2
    ;;
  esac
}

for shard in "${SELECTED_SHARDS[@]}"; do
  evidence_key=$(shard_evidence_key "$shard")
  pass_file="$EVIDENCE_DIR/$shard.$evidence_key.pass"
  log_file="$EVIDENCE_DIR/$shard.$evidence_key.log"

  rm -f "$EVIDENCE_DIR/$shard".*.fail
  started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  started_epoch=$(date +%s)
  printf 'START shard=%s at=%s key=%s\n' "$shard" "$started_at" "$evidence_key"
  if run_shard_command "$shard" 2>&1 | tee "$log_file"; then
    finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    duration_seconds=$(($(date +%s) - started_epoch))
    cat >"$pass_file" <<EOF_PASS
result=pass
shard=$shard
evidence_key=$evidence_key
candidate_fingerprint=$CANDIDATE_FINGERPRINT
runner_sha256=$RUNNER_SHA256
postgres_image=$POSTGRES_IMAGE
go_version=$GO_VERSION
test_run_regex=$TEST_RUN_REGEX
started_at=$started_at
finished_at=$finished_at
duration_seconds=$duration_seconds
log_file=$log_file
EOF_PASS
    printf 'PASS shard=%s duration_seconds=%s key=%s\n' "$shard" "$duration_seconds" "$evidence_key"
  else
    status=${PIPESTATUS[0]}
    finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    duration_seconds=$(($(date +%s) - started_epoch))
    fail_file="$EVIDENCE_DIR/$shard.$evidence_key.fail"
    cat >"$fail_file" <<EOF_FAIL
result=fail
exit_code=$status
shard=$shard
evidence_key=$evidence_key
candidate_fingerprint=$CANDIDATE_FINGERPRINT
runner_sha256=$RUNNER_SHA256
postgres_image=$POSTGRES_IMAGE
go_version=$GO_VERSION
test_run_regex=$TEST_RUN_REGEX
started_at=$started_at
finished_at=$finished_at
duration_seconds=$duration_seconds
log_file=$log_file
EOF_FAIL
    printf 'FAIL shard=%s exit_code=%s duration_seconds=%s key=%s\n' \
      "$shard" "$status" "$duration_seconds" "$evidence_key" >&2
    exit "$status"
  fi
done

printf 'COMPLETE shards=%s\n' "${SELECTED_SHARDS[*]}"
