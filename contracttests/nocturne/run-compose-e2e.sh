#!/bin/sh
set -eu
umask 077

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
COMPOSE_FILE="$ROOT/deploy/compose.yaml"
OCI_LAYOUT=${1:-}
SCENARIO=${2:-full}

if [ -z "$OCI_LAYOUT" ] || [ ! -d "$OCI_LAYOUT" ] || { [ "$SCENARIO" != full ] && [ "$SCENARIO" != rollback ] && [ "$SCENARIO" != backup ]; }; then
  printf '%s\n' "usage: $0 /absolute/path/to/verified-oci-layout [full|rollback|backup]" >&2
  exit 2
fi
for command in docker skopeo python3 go; do
  command -v "$command" >/dev/null 2>&1 || {
    printf '%s\n' "required command is unavailable: $command" >&2
    exit 2
  }
done

docker compose version >/dev/null
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/edu-agent-nocturne-e2e.XXXXXX")
PROJECT="edu-agent-nocturne-e2e-$$"
ENV_FILE="$TMP_DIR/e2e.env"
OVERRIDE_FILE="$TMP_DIR/compose.override.yaml"
REGISTRY_NAME="$PROJECT-registry"
REGISTRY_IMAGE="registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  set +e
  if [ "$status" -ne 0 ]; then
    docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" --env-file "$ENV_FILE" -p "$PROJECT" logs --no-color --tail=200 server nocturne >&2
  fi
  docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" --env-file "$ENV_FILE" -p "$PROJECT" down --volumes --remove-orphans >/dev/null 2>&1
  docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" | while IFS= read -r container; do
    [ -z "$container" ] || docker rm -f "$container" >/dev/null 2>&1
  done
  docker volume ls -q --filter "label=edu-agent.nocturne.rollback.project=$PROJECT" | while IFS= read -r volume; do
    [ -z "$volume" ] || docker volume rm "$volume" >/dev/null 2>&1
  done
  if [ -n "${NOCTURNE_IMAGE_REF:-}" ]; then
    docker image rm "$NOCTURNE_IMAGE_REF" >/dev/null 2>&1
  fi
  docker rm -f "$REGISTRY_NAME" >/dev/null 2>&1
  rm -rf "$TMP_DIR"
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

python3 "$ROOT/deploy/nocturne/scripts/tool.py" verify-lock >/dev/null
python3 "$ROOT/deploy/nocturne/scripts/tool.py" verify-oci "$OCI_LAYOUT" >/dev/null
IMAGE_DIGEST=$(
  python3 - "$ROOT/deploy/nocturne/image.lock.json" <<'PY'
import json
import sys
print(json.load(open(sys.argv[1], encoding="utf-8"))["platform_manifest_digest"])
PY
)

REGISTRY_PORT=$(
  python3 - <<'PY'
import socket
with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PY
)
docker run -d --name "$REGISTRY_NAME" -p "127.0.0.1:$REGISTRY_PORT:5000" "$REGISTRY_IMAGE" >/dev/null
REGISTRY_REPOSITORY="127.0.0.1:$REGISTRY_PORT/edu-agent/nocturne"
skopeo copy --preserve-digests --dest-tls-verify=false "oci:$OCI_LAYOUT" "docker://$REGISTRY_REPOSITORY:verified" >/dev/null
NOCTURNE_IMAGE_REF="$REGISTRY_REPOSITORY@$IMAGE_DIGEST"
docker pull "$NOCTURNE_IMAGE_REF" >/dev/null
python3 - "$ENV_FILE" "$NOCTURNE_IMAGE_REF" <<'PY'
import base64
import secrets
import socket
import sys
from pathlib import Path

def token() -> str:
    return base64.urlsafe_b64encode(secrets.token_bytes(32)).decode().rstrip("=")

with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    port = listener.getsockname()[1]

values = {
    "POSTGRES_PASSWORD": token(),
    "SERVER_PORT": str(port),
    "MODEL_REQUIRED": "false",
    "MODEL_BASE_URL": "",
    "MODEL_NAME": "",
    "MODEL_API_KEY": "",
    "MODEL_CONTEXT_WINDOW": "",
    "NOCTURNE_IMAGE": sys.argv[2],
    "NOCTURNE_POSTGRES_DB": "nocturne",
    "NOCTURNE_POSTGRES_USER": "nocturne",
    "NOCTURNE_POSTGRES_PASSWORD": token(),
    "NOCTURNE_BRIDGE_API_TOKEN": token(),
    "NOCTURNE_MAINTENANCE_TOKEN": token(),
    "NOCTURNE_BACKUP_MASTER_WRAPPING_KEY": token(),
    "NOCTURNE_BACKUP_CONTROLLER_INTERVAL": "2s",
    "NOCTURNE_BACKUP_RETENTION": "90s",
}
Path(sys.argv[1]).write_text("".join(f"{key}={value}\n" for key, value in values.items()), encoding="utf-8")
PY

cat >"$OVERRIDE_FILE" <<'EOF'
services:
  nocturne:
    pull_policy: never
  server:
    environment:
      NOCTURNE_HTTP_TIMEOUT: 1s
      NOCTURNE_RECONCILIATION_INTERVAL: 1s
      NOCTURNE_WORKER_POLL_INTERVAL: 1s
      NOCTURNE_WORKER_LEASE_DURATION: 12s
      NOCTURNE_DELIVERY_TTL: 12s
      NOCTURNE_CANDIDATE_SWEEP_INTERVAL: 1s
      NOCTURNE_DELIVERY_SWEEP_INTERVAL: 1s
EOF

(
  cd "$ROOT/server"
  go test ./internal/integrations/nocturne -run 'TestManagedBackupRoundTripChunkBoundariesAndDestroyedRestore|TestManagedBackupErasureVerificationDestroyedArtifactSucceedsAndLiveKeyFails|TestManagedBackupPrecisePruneSuccess' -count=1
)

docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" --env-file "$ENV_FILE" -p "$PROJECT" up -d --build
python3 "$ROOT/contracttests/nocturne/compose_e2e.py" \
  --compose-file "$COMPOSE_FILE" \
  --override-file "$OVERRIDE_FILE" \
  --env-file "$ENV_FILE" \
  --project "$PROJECT" \
  --scenario "$SCENARIO"
