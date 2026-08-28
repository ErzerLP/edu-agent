#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
export GOFLAGS=''

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
SERVER_DIR="$ROOT/server"
CLI_CONTRACT_DIR="$ROOT/contracttests/cli-m1"
APPROVED_POSTGRES_IMAGE='postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73'
APPROVED_POSTGRES_PLATFORM='linux/amd64'
APPROVED_GO_VERSION='go1.26.6'
POSTGRES_IMAGE=${POSTGRES_IMAGE:-$APPROVED_POSTGRES_IMAGE}
POSTGRES_PLATFORM=${POSTGRES_PLATFORM:-$APPROVED_POSTGRES_PLATFORM}
POSTGRES_TEST_TIMEOUT=${POSTGRES_TEST_TIMEOUT:-45m}
POSTGRES_TMPFS_SIZE=${POSTGRES_TMPFS_SIZE:-2g}
LOCK_FILE=${OPERATIONS_CANDIDATE_LOCK_FILE:-/tmp/edu-agent-operations-candidate.lock}
LOCK_FD=${OPERATIONS_CANDIDATE_LOCK_FD:-}
LOCK_PROTOCOL=${OPERATIONS_CANDIDATE_LOCK_PROTOCOL:-}
ALL_SHARDS=(db-core learning-core learning-offline learning-fault memory privacy-core privacy-fault model-vertical offline-blackbox)
SELECTED_SHARDS=()
RESUME=0
LIST_ONLY=0
EVIDENCE_DIR=${POSTGRES_EVIDENCE_DIR:-}
TEST_RUN_REGEX=${POSTGRES_TEST_RUN:-}
CONTAINER_NAME=""
SHARD_CWD=""
SHARD_REGEX=""
SHARD_PACKAGES=()
declare -A SHARD_KEYS=()
declare -A SHARD_SELECTION_FILES=()
declare -A SHARD_EXPECTED_FILES=()
declare -A SHARD_SELECTION_SHA256=()
declare -A SHARD_EXPECTED_SHA256=()
declare -A SHARD_REGEXES=()
declare -A SHARD_CWDS=()
declare -A SHARD_PACKAGE_LISTS=()

usage() {
  cat <<'USAGE'
usage: scripts/test-postgres-candidate.sh [options]

Run the real PostgreSQL candidate matrix as stable, strictly serial shards.

Options:
  --shard NAME       Run one shard; repeat to select multiple shards.
  --resume           Reuse only a matching marker with valid selection/log hashes.
  --evidence-dir DIR Store redacted logs, selections, and markers in DIR.
  --run REGEX        Override the selected shard's Go test regex.
  --list             List shard names and exit.
  -h, --help         Show this help.

Environment:
  POSTGRES_IMAGE                Must equal the approved digest-pinned PostgreSQL image.
  POSTGRES_PLATFORM             Must equal the approved image platform.
  POSTGRES_TEST_TIMEOUT         Per-shard Go timeout (default: 45m).
  POSTGRES_TMPFS_SIZE           PostgreSQL tmpfs size (default: 2g).
  POSTGRES_EVIDENCE_DIR         Default evidence directory.
  POSTGRES_TEST_RUN             Default Go test regex override.
  OPERATIONS_CANDIDATE_LOCK_FILE  Shared host qualification lock (default: /tmp/edu-agent-operations-candidate.lock).
  OPERATIONS_CANDIDATE_LOCK_FD    Inherited locked descriptor supplied by the coordinator or parent gate.
  CANDIDATE_ID                    Required for source archives without .git metadata.
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

for command in docker flock go git readlink sha256sum sort; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 2
  }
done
for shard in "${SELECTED_SHARDS[@]}"; do
  case "$shard" in
  model-vertical | offline-blackbox)
    command -v psql >/dev/null 2>&1 || {
      echo "required command is unavailable: psql" >&2
      exit 2
    }
    break
    ;;
  esac
done
LOCK_FILE=$(readlink -m -- "$LOCK_FILE")
[[ -d "$SERVER_DIR" && -d "$CLI_CONTRACT_DIR" ]] || {
  echo "required Go module directory is unavailable" >&2
  exit 2
}
[[ "$POSTGRES_IMAGE" == "$APPROVED_POSTGRES_IMAGE" ]] || {
  echo "POSTGRES_IMAGE must equal the approved digest: $APPROVED_POSTGRES_IMAGE" >&2
  exit 2
}
[[ "$POSTGRES_PLATFORM" == "$APPROVED_POSTGRES_PLATFORM" ]] || {
  echo "POSTGRES_PLATFORM must equal the approved platform: $APPROVED_POSTGRES_PLATFORM" >&2
  exit 2
}
GO_VERSION=$(go version)
[[ " $GO_VERSION " == *" $APPROVED_GO_VERSION "* ]] || {
  echo "required Go toolchain is unavailable: $APPROVED_GO_VERSION" >&2
  exit 2
}

candidate_fingerprint() {
  local -a paths=(server clients contracttests scripts deploy Makefile go.mod go.sum go.work go.work.sum .go-version .tool-versions)
  (
    cd "$ROOT"
    {
      git rev-parse HEAD
      git diff HEAD --binary --no-ext-diff -- "${paths[@]}"
      while IFS= read -r -d '' path; do
        printf '%s\0' "$path"
        if [[ -e "$path" || -L "$path" ]]; then
          sha256sum "$path"
        else
          printf '%s  %s\n' missing "$path"
        fi
      done < <(git ls-files -z --others --exclude-standard -- "${paths[@]}" | LC_ALL=C sort -z)
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
if [[ -z "$EVIDENCE_DIR" ]]; then
  EVIDENCE_DIR="${TMPDIR:-/tmp}/edu-agent-postgres-evidence/${CANDIDATE_ID:-$CANDIDATE_FINGERPRINT}"
fi
mkdir -p "$EVIDENCE_DIR"
chmod 700 "$EVIDENCE_DIR"

operations_helper() {
  (cd "$ROOT/contracttests/operations" && go run ./cmd/operations-candidate "$@")
}

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

configure_shard() {
  local shard=$1
  SHARD_CWD="$SERVER_DIR"
  SHARD_PACKAGES=()
  case "$shard" in
  db-core)
    SHARD_REGEX=${TEST_RUN_REGEX:-^Test}
    SHARD_PACKAGES=(
      ./internal/app
      ./internal/knowledge/postgresstore
      ./internal/identity/postgresstore
      ./internal/platform/outbox/postgresstore
      ./internal/platform/postgres
      ./migrations
    )
    ;;
  learning-core)
    SHARD_REGEX=${TEST_RUN_REGEX:-$(list_test_regex ./internal/learning/postgresstore learning-core)}
    SHARD_PACKAGES=(./internal/learning/postgresstore)
    ;;
  learning-offline)
    SHARD_REGEX=${TEST_RUN_REGEX:-$(list_test_regex ./internal/learning/postgresstore learning-offline)}
    SHARD_PACKAGES=(./internal/learning/postgresstore)
    ;;
  learning-fault)
    SHARD_REGEX=${TEST_RUN_REGEX:-^TestPostgreSQLTypedRecordWriteFaultMatrixRollsBack$}
    SHARD_PACKAGES=(./internal/learning/postgresstore)
    ;;
  memory)
    SHARD_REGEX=${TEST_RUN_REGEX:-^Test}
    SHARD_PACKAGES=(./internal/memory/postgresstore)
    ;;
  privacy-core)
    SHARD_REGEX=${TEST_RUN_REGEX:-$(list_test_regex ./internal/privacy/postgresstore privacy-core)}
    SHARD_PACKAGES=(./internal/privacy/postgresstore)
    ;;
  privacy-fault)
    SHARD_REGEX=${TEST_RUN_REGEX:-^TestPostgreSQLLocalPrivacyScrubFaultMatrix$}
    SHARD_PACKAGES=(./internal/privacy/postgresstore)
    ;;
  model-vertical)
    SHARD_CWD="$CLI_CONTRACT_DIR"
    SHARD_REGEX=${TEST_RUN_REGEX:-^TestBlackBoxProductionFakeModelVerticalPostgreSQL$}
    SHARD_PACKAGES=(./blackbox)
    ;;
  offline-blackbox)
    SHARD_CWD="$CLI_CONTRACT_DIR"
    SHARD_REGEX=${TEST_RUN_REGEX:-^TestBlackBoxOfflineObjectivePrepareLearnSyncStatus$}
    SHARD_PACKAGES=(./blackbox)
    ;;
  *)
    echo "unsupported shard: $shard" >&2
    return 2
    ;;
  esac
}

prepare_selection() {
  local shard=$1
  local temporary="$EVIDENCE_DIR/.${shard}.selected.$$"
  : >"$temporary"
  local package import_path name
  for package in "${SHARD_PACKAGES[@]}"; do
    import_path=$(cd "$SHARD_CWD" && go list -f '{{.ImportPath}}' "$package")
    while IFS= read -r name; do
      [[ "$name" =~ ^Test[[:alnum:]_]+$ ]] || continue
      printf '%s\t%s\n' "$import_path" "$name" >>"$temporary"
    done < <(cd "$SHARD_CWD" && go test -list "$SHARD_REGEX" "$package")
  done
  LC_ALL=C sort -u -o "$temporary" "$temporary"
  [[ -s "$temporary" ]] || {
    rm -f "$temporary"
    echo "no tests selected for shard $shard" >&2
    return 1
  }
  local expected_temporary="${temporary}.expected"
  if [[ -n ${OPERATIONS_EXPECTED_GO_TESTS:-} ]]; then
    : >"$expected_temporary"
    local expected_name matched
    local -a expected_names=()
    IFS=',' read -r -a expected_names <<<"$OPERATIONS_EXPECTED_GO_TESTS"
    ((${#expected_names[@]} > 0)) || {
      rm -f "$temporary" "$expected_temporary"
      echo "operations expected Go target list is empty" >&2
      return 1
    }
    for expected_name in "${expected_names[@]}"; do
      [[ "$expected_name" =~ ^Test[[:alnum:]_]+$ ]] || {
        rm -f "$temporary" "$expected_temporary"
        echo "invalid operations expected Go target: $expected_name" >&2
        return 1
      }
      if ! matched=$(awk -F '\t' -v target="$expected_name" '
        $2 == target { count++; value = $0 }
        END { if (count != 1) exit 1; print value }
      ' "$temporary"); then
        rm -f "$temporary" "$expected_temporary"
        echo "operations expected Go target is missing or ambiguous: $expected_name" >&2
        return 1
      fi
      printf '%s\n' "$matched" >>"$expected_temporary"
    done
    LC_ALL=C sort -u -o "$expected_temporary" "$expected_temporary"
  else
    cp "$temporary" "$expected_temporary"
  fi

  local selection_sha expected_sha evidence_key selection_file expected_file
  selection_sha=$(sha256sum "$temporary" | awk '{print $1}')
  expected_sha=$(sha256sum "$expected_temporary" | awk '{print $1}')
  evidence_key=$(
    printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' \
      "$CANDIDATE_FINGERPRINT" "$RUNNER_SHA256" "$POSTGRES_IMAGE" "$POSTGRES_PLATFORM" \
      "$GO_VERSION" "$shard" "$SHARD_REGEX" "$selection_sha" "$expected_sha" |
      sha256sum | awk '{print $1}'
  )
  selection_file="$EVIDENCE_DIR/$shard.$evidence_key.selected"
  expected_file="$EVIDENCE_DIR/$shard.$evidence_key.expected"
  mv -f "$temporary" "$selection_file"
  mv -f "$expected_temporary" "$expected_file"
  chmod 600 "$selection_file" "$expected_file"
  SHARD_KEYS["$shard"]=$evidence_key
  SHARD_SELECTION_FILES["$shard"]=$selection_file
  SHARD_EXPECTED_FILES["$shard"]=$expected_file
  SHARD_SELECTION_SHA256["$shard"]=$selection_sha
  SHARD_EXPECTED_SHA256["$shard"]=$expected_sha
  SHARD_REGEXES["$shard"]=$SHARD_REGEX
  SHARD_CWDS["$shard"]=$SHARD_CWD
  SHARD_PACKAGE_LISTS["$shard"]=$(printf '%s ' "${SHARD_PACKAGES[@]}")
}

verify_pass_marker() {
  local shard=$1
  local marker=$2
  local expected_key=${SHARD_KEYS[$shard]}
  local expected_selection_sha=${SHARD_SELECTION_SHA256[$shard]}
  local expected_operations_sha=${SHARD_EXPECTED_SHA256[$shard]}
  [[ -f "$marker" ]] || return 1
  declare -A fields=()
  local key value
  while IFS='=' read -r key value; do
    case "$key" in
    schema_version | status | shard | evidence_key | candidate_fingerprint | runner_sha256 | postgres_image | postgres_platform | go_version | test_run_regex | selection_sha256 | expected_selection_sha256 | log_sha256 | log_bytes | log_file | started_at | finished_at | duration_seconds)
      ;;
    *) return 1 ;;
    esac
    [[ -z ${fields[$key]+x} ]] || return 1
    fields["$key"]=$value
  done <"$marker"
  ((${#fields[@]} == 18)) || return 1
  [[ ${fields[schema_version]:-} == 'edu-agent.postgres-candidate-evidence/v2' ]] || return 1
  [[ ${fields[status]:-} == passed ]] || return 1
  [[ ${fields[shard]:-} == "$shard" ]] || return 1
  [[ ${fields[evidence_key]:-} == "$expected_key" ]] || return 1
  [[ ${fields[candidate_fingerprint]:-} == "$CANDIDATE_FINGERPRINT" ]] || return 1
  [[ ${fields[runner_sha256]:-} == "$RUNNER_SHA256" ]] || return 1
  [[ ${fields[postgres_image]:-} == "$POSTGRES_IMAGE" ]] || return 1
  [[ ${fields[postgres_platform]:-} == "$POSTGRES_PLATFORM" ]] || return 1
  [[ ${fields[go_version]:-} == "$GO_VERSION" ]] || return 1
  [[ ${fields[test_run_regex]:-} == "${SHARD_REGEXES[$shard]}" ]] || return 1
  [[ ${fields[selection_sha256]:-} == "$expected_selection_sha" ]] || return 1
  [[ ${fields[expected_selection_sha256]:-} == "$expected_operations_sha" ]] || return 1
  [[ -n ${fields[started_at]:-} && -n ${fields[finished_at]:-} && ${fields[duration_seconds]:-} =~ ^[0-9]+$ ]] || return 1
  local log_file=${fields[log_file]:-}
  [[ -f "$log_file" ]] || return 1
  [[ $(sha256sum "$log_file" | awk '{print $1}') == "${fields[log_sha256]:-}" ]] || return 1
  [[ $(wc -c <"$log_file" | tr -d ' ') == "${fields[log_bytes]:-}" ]] || return 1
  operations_helper verify-go-events --log "$log_file" --selected-file "${SHARD_EXPECTED_FILES[$shard]}" >/dev/null
}

acquire_operations_lock() {
  if [[ -n "$LOCK_FD" ]]; then
    [[ "$LOCK_PROTOCOL" == 'inherited-fd-v1' && "$LOCK_FD" =~ ^[0-9]+$ ]] || {
      echo "invalid inherited operations candidate lock protocol" >&2
      exit 1
    }
    [[ -e "/proc/$$/fd/$LOCK_FD" ]] || {
      echo "inherited operations candidate lock descriptor is unavailable" >&2
      exit 1
    }
    local inherited_target expected_target
    inherited_target=$(readlink -f -- "/proc/$$/fd/$LOCK_FD")
    expected_target=$(readlink -f -- "$LOCK_FILE")
    [[ "$inherited_target" == "$expected_target" ]] || {
      echo "inherited operations candidate lock descriptor targets the wrong file" >&2
      exit 1
    }
    flock -n "$LOCK_FD" || {
      echo "inherited operations candidate lock is not held" >&2
      exit 1
    }
    return
  fi
  exec 9>"$LOCK_FILE"
  if ! flock -n 9; then
    echo "another host-wide candidate gate holds $LOCK_FILE" >&2
    exit 2
  fi
  export OPERATIONS_CANDIDATE_LOCK_FILE="$LOCK_FILE"
  export OPERATIONS_CANDIDATE_LOCK_FD=9
  export OPERATIONS_CANDIDATE_LOCK_PROTOCOL='inherited-fd-v1'
}

printf 'candidate_fingerprint=%s\n' "$CANDIDATE_FINGERPRINT"
printf 'runner_sha256=%s\n' "$RUNNER_SHA256"
printf 'postgres_image=%s\n' "$POSTGRES_IMAGE"
printf 'postgres_platform=%s\n' "$POSTGRES_PLATFORM"
printf 'go_version=%s\n' "$GO_VERSION"
printf 'evidence_dir=%s\n' "$EVIDENCE_DIR"

acquire_operations_lock

PENDING_SHARDS=()
for shard in "${SELECTED_SHARDS[@]}"; do
  configure_shard "$shard"
  prepare_selection "$shard"
  evidence_key=${SHARD_KEYS[$shard]}
  pass_file="$EVIDENCE_DIR/$shard.$evidence_key.pass"
  if ((RESUME)) && verify_pass_marker "$shard" "$pass_file"; then
    printf 'REUSED shard=%s reason=verified-pass-evidence key=%s\n' "$shard" "$evidence_key"
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
  --platform "$POSTGRES_PLATFORM" \
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

run_shard_command() {
  local shard=$1
  local cwd=${SHARD_CWDS[$shard]}
  local regex=${SHARD_REGEXES[$shard]}
  local package_string=${SHARD_PACKAGE_LISTS[$shard]}
  local -a packages=()
  if [[ -n "$TEST_RUN_REGEX" ]]; then
    mapfile -t packages < <(cut -f1 "${SHARD_SELECTION_FILES[$shard]}" | LC_ALL=C sort -u)
  else
    read -r -a packages <<<"$package_string"
  fi
  ((${#packages[@]} > 0)) || {
    echo "no packages selected for shard $shard" >&2
    return 1
  }
  (cd "$cwd" && go test -json -p=1 -count=1 -timeout="$POSTGRES_TEST_TIMEOUT" -run "$regex" "${packages[@]}")
}

for shard in "${SELECTED_SHARDS[@]}"; do
  evidence_key=${SHARD_KEYS[$shard]}
  pass_file="$EVIDENCE_DIR/$shard.$evidence_key.pass"
  fail_file="$EVIDENCE_DIR/$shard.$evidence_key.fail"
  log_file="$EVIDENCE_DIR/$shard.$evidence_key.log"
  log_temp="$EVIDENCE_DIR/.$shard.$evidence_key.log.$$"
  rm -f "$EVIDENCE_DIR/$shard".*.fail "$log_temp"
  started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  started_epoch=$(date +%s)
  printf 'START shard=%s at=%s key=%s\n' "$shard" "$started_at" "$evidence_key"

  set +e
  run_shard_command "$shard" 2>&1 | operations_helper redact-stream | tee "$log_temp"
  pipeline_status=("${PIPESTATUS[@]}")
  set -e
  run_status=${pipeline_status[0]}
  redact_status=${pipeline_status[1]}
  tee_status=${pipeline_status[2]}
  mv -f "$log_temp" "$log_file"
  chmod 600 "$log_file"

  verification_status=0
  if ((run_status == 0 && redact_status == 0 && tee_status == 0)); then
    if ! operations_helper verify-go-events --log "$log_file" --selected-file "${SHARD_EXPECTED_FILES[$shard]}"; then
      verification_status=1
    fi
  fi
  finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  duration_seconds=$(($(date +%s) - started_epoch))
  log_sha256=$(sha256sum "$log_file" | awk '{print $1}')
  log_bytes=$(wc -c <"$log_file" | tr -d ' ')

  if ((run_status == 0 && redact_status == 0 && tee_status == 0 && verification_status == 0)); then
    marker_temp="$pass_file.tmp.$$"
    cat >"$marker_temp" <<EOF_PASS
schema_version=edu-agent.postgres-candidate-evidence/v2
status=passed
shard=$shard
evidence_key=$evidence_key
candidate_fingerprint=$CANDIDATE_FINGERPRINT
runner_sha256=$RUNNER_SHA256
postgres_image=$POSTGRES_IMAGE
postgres_platform=$POSTGRES_PLATFORM
go_version=$GO_VERSION
test_run_regex=${SHARD_REGEXES[$shard]}
selection_sha256=${SHARD_SELECTION_SHA256[$shard]}
expected_selection_sha256=${SHARD_EXPECTED_SHA256[$shard]}
log_sha256=$log_sha256
log_bytes=$log_bytes
log_file=$log_file
started_at=$started_at
finished_at=$finished_at
duration_seconds=$duration_seconds
EOF_PASS
    chmod 600 "$marker_temp"
    mv -f "$marker_temp" "$pass_file"
    rm -f "$fail_file"
    printf 'PASS shard=%s duration_seconds=%s key=%s\n' "$shard" "$duration_seconds" "$evidence_key"
  else
    status=$run_status
    ((status != 0)) || status=$redact_status
    ((status != 0)) || status=$tee_status
    ((status != 0)) || status=$verification_status
    marker_temp="$fail_file.tmp.$$"
    cat >"$marker_temp" <<EOF_FAIL
schema_version=edu-agent.postgres-candidate-evidence/v2
status=failed
exit_code=$status
shard=$shard
evidence_key=$evidence_key
candidate_fingerprint=$CANDIDATE_FINGERPRINT
runner_sha256=$RUNNER_SHA256
postgres_image=$POSTGRES_IMAGE
postgres_platform=$POSTGRES_PLATFORM
go_version=$GO_VERSION
test_run_regex=${SHARD_REGEXES[$shard]}
selection_sha256=${SHARD_SELECTION_SHA256[$shard]}
expected_selection_sha256=${SHARD_EXPECTED_SHA256[$shard]}
log_sha256=$log_sha256
log_bytes=$log_bytes
log_file=$log_file
EOF_FAIL
    chmod 600 "$marker_temp"
    mv -f "$marker_temp" "$fail_file"
    rm -f "$pass_file"
    printf 'FAIL shard=%s exit_code=%s duration_seconds=%s key=%s\n' \
      "$shard" "$status" "$duration_seconds" "$evidence_key" >&2
    exit "$status"
  fi
done

printf 'COMPLETE shards=%s\n' "${SELECTED_SHARDS[*]}"
