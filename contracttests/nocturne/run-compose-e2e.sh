#!/bin/sh
set -eu
umask 077

ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
COMPOSE_FILE="$ROOT/deploy/compose.yaml"
OCI_LAYOUT=${1:-}
SCENARIO=${2:-full}

if [ -z "$OCI_LAYOUT" ] || [ ! -d "$OCI_LAYOUT" ] || { [ "$SCENARIO" != full ] && [ "$SCENARIO" != rollback ] && [ "$SCENARIO" != backup ] && [ "$SCENARIO" != expiry ] && [ "$SCENARIO" != replay ]; }; then
  printf '%s\n' "usage: $0 /absolute/path/to/verified-oci-layout [full|rollback|backup|expiry|replay]" >&2
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
FAILED_FORWARD_TAG=""
FAILED_FORWARD_IMAGE_REF=""
SERVER_IMAGE_SOURCE=${NOCTURNE_E2E_SERVER_IMAGE:-}
SERVER_IMAGE_REF=""
GATE_LOG="${NOCTURNE_E2E_GATE_LOG:-${TMPDIR:-/tmp}/edu-agent-nocturne-compose-e2e-last.log}"
COMPOSE_LOG="${NOCTURNE_E2E_COMPOSE_LOG:-${TMPDIR:-/tmp}/edu-agent-nocturne-compose-e2e-compose-last.log}"

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  set +e
  if [ "$status" -ne 0 ]; then
    docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" --env-file "$ENV_FILE" -p "$PROJECT" logs --no-color --tail=200 server nocturne >"$COMPOSE_LOG" 2>&1
    cat "$COMPOSE_LOG" >&2
    printf 'Nocturne Compose service logs preserved at %s\n' "$COMPOSE_LOG" >&2
  fi
  if [ -f "$ENV_FILE" ] && [ -f "$OVERRIDE_FILE" ]; then
    docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" --env-file "$ENV_FILE" -p "$PROJECT" down --volumes --remove-orphans >/dev/null 2>&1
  fi
  docker ps -aq --filter "label=com.docker.compose.project=$PROJECT" | while IFS= read -r container; do
    [ -z "$container" ] || docker rm -f "$container" >/dev/null 2>&1
  done
  docker volume ls -q --filter "label=edu-agent.nocturne.rollback.project=$PROJECT" | while IFS= read -r volume; do
    [ -z "$volume" ] || docker volume rm "$volume" >/dev/null 2>&1
  done
  if [ -n "${FAILED_FORWARD_IMAGE_REF:-}" ]; then
    docker image rm "$FAILED_FORWARD_IMAGE_REF" >/dev/null 2>&1
  fi
  if [ -n "${FAILED_FORWARD_TAG:-}" ]; then
    docker image rm "$FAILED_FORWARD_TAG" >/dev/null 2>&1
  fi
  if [ -n "${NOCTURNE_IMAGE_REF:-}" ]; then
    docker image rm "$NOCTURNE_IMAGE_REF" >/dev/null 2>&1
  fi
  if [ -n "${SERVER_IMAGE_REF:-}" ]; then
    docker image rm "$SERVER_IMAGE_REF" >/dev/null 2>&1
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
python3 - "$REGISTRY_PORT" <<'PY'
import sys
import time
import urllib.error
import urllib.request

url = f"http://127.0.0.1:{sys.argv[1]}/v2/"
for _ in range(60):
    try:
        with urllib.request.urlopen(url, timeout=0.5) as response:
            if response.status == 200:
                break
    except (OSError, urllib.error.URLError):
        pass
    time.sleep(0.25)
else:
    raise SystemExit("local test registry did not become ready")
PY
REGISTRY_REPOSITORY="127.0.0.1:$REGISTRY_PORT/edu-agent/nocturne"
skopeo copy --preserve-digests --dest-tls-verify=false "oci:$OCI_LAYOUT" "docker://$REGISTRY_REPOSITORY:verified" >/dev/null
NOCTURNE_IMAGE_REF="$REGISTRY_REPOSITORY@$IMAGE_DIGEST"
docker pull "$NOCTURNE_IMAGE_REF" >/dev/null
SERVER_IMAGE_REF="$PROJECT-server:candidate"
if [ -n "$SERVER_IMAGE_SOURCE" ]; then
  docker image inspect "$SERVER_IMAGE_SOURCE" >/dev/null
  docker tag "$SERVER_IMAGE_SOURCE" "$SERVER_IMAGE_REF"
else
  docker build --provenance=false --sbom=false \
    -f "$ROOT/server/Dockerfile" \
    -t "$SERVER_IMAGE_REF" "$ROOT"
fi
FAILED_FORWARD_FIXTURE_SHA256=$(
  python3 - "$ROOT/contracttests/nocturne/failed_forward.Dockerfile" "$ROOT/contracttests/nocturne/failed_forward.py" <<'PY'
import hashlib
import sys
h = hashlib.sha256()
for path in sys.argv[1:]:
    with open(path, "rb") as source:
        h.update(path.rsplit("/", 1)[-1].encode("ascii") + b"\0" + source.read() + b"\0")
print(h.hexdigest())
PY
)
FAILED_FORWARD_TAG="$REGISTRY_REPOSITORY-failed-forward:a84-v1"
docker build --no-cache --provenance=false --sbom=false \
  --build-arg "BASE_IMAGE=$NOCTURNE_IMAGE_REF" \
  --build-arg "SOURCE_DATE_EPOCH=1754006400" \
  --label "edu-agent.nocturne.failed-forward-fixture-sha256=$FAILED_FORWARD_FIXTURE_SHA256" \
  -f "$ROOT/contracttests/nocturne/failed_forward.Dockerfile" \
  -t "$FAILED_FORWARD_TAG" "$ROOT/contracttests/nocturne" >/dev/null
docker push "$FAILED_FORWARD_TAG" >/dev/null
docker buildx imagetools inspect --raw "$FAILED_FORWARD_TAG" >"$TMP_DIR/failed-forward-manifest.json"
FAILED_FORWARD_IMAGE_DIGEST=$(
  python3 - "$TMP_DIR/failed-forward-manifest.json" <<'PY'
import hashlib
import sys
print("sha256:" + hashlib.sha256(open(sys.argv[1], "rb").read()).hexdigest())
PY
)
FAILED_FORWARD_CONFIG_DIGEST=$(
  python3 - "$TMP_DIR/failed-forward-manifest.json" <<'PY'
import json
import sys
print(json.load(open(sys.argv[1], encoding="utf-8"))["config"]["digest"])
PY
)
FAILED_FORWARD_IMAGE_REF="$REGISTRY_REPOSITORY-failed-forward@$FAILED_FORWARD_IMAGE_DIGEST"
[ "$FAILED_FORWARD_IMAGE_DIGEST" != "$IMAGE_DIGEST" ] || {
  printf '%s\n' "failed-forward image did not produce a distinct digest" >&2
  exit 1
}
docker pull "$FAILED_FORWARD_IMAGE_REF" >/dev/null
python3 - "$ENV_FILE" "$NOCTURNE_IMAGE_REF" "$FAILED_FORWARD_IMAGE_REF" "$FAILED_FORWARD_CONFIG_DIGEST" "$FAILED_FORWARD_FIXTURE_SHA256" "$SERVER_IMAGE_REF" <<'PY'
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
    "OFFLINE_SIGNER_KEY_ID": "compose-e2e-offline-signer",
    "OFFLINE_SIGNER_PRIVATE_KEY": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8DoQe_884Qvh1w3RjnS8CZZ-TWMJulDV8d3IZkElUxuA",  #gitleaks:allow
    "OFFLINE_SIGNER_ISSUED_AT": "2026-01-01T00:00:00Z",
    "OFFLINE_SIGNER_NOT_AFTER": "2030-01-01T00:00:00Z",
    "PRIVACY_OFFLINE_CHALLENGE_KEYS": "2:" + base64.urlsafe_b64encode(bytes(32)).decode().rstrip("="),
    "NOCTURNE_IMAGE": sys.argv[2],
    "NOCTURNE_FAILED_FORWARD_IMAGE": sys.argv[3],
    "NOCTURNE_FAILED_FORWARD_CONFIG_DIGEST": sys.argv[4],
    "NOCTURNE_FAILED_FORWARD_FIXTURE_SHA256": sys.argv[5],
    "NOCTURNE_POSTGRES_DB": "nocturne",
    "NOCTURNE_POSTGRES_USER": "nocturne",
    "NOCTURNE_POSTGRES_PASSWORD": token(),
    "NOCTURNE_BRIDGE_API_TOKEN": token(),
    "NOCTURNE_MAINTENANCE_TOKEN": token(),
    "NOCTURNE_BACKUP_MASTER_WRAPPING_KEY": token(),
    "NOCTURNE_BACKUP_CONTROLLER_INTERVAL": "2s",
    "NOCTURNE_BACKUP_RETENTION": "90s",
    "SERVER_IMAGE": sys.argv[6],
}
Path(sys.argv[1]).write_text("".join(f"{key}={value}\n" for key, value in values.items()), encoding="utf-8")
PY

cat >"$OVERRIDE_FILE" <<'EOF'
services:
  nocturne:
    pull_policy: never
    healthcheck:
      test:
        [
          "CMD",
          "python",
          "-c",
          "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8233/health', timeout=12).read()",
        ]
      interval: 5s
      timeout: 15s
      retries: 24
      start_period: 10s
  server:
    image: ${SERVER_IMAGE:?set SERVER_IMAGE to the prepared local server candidate}
    pull_policy: never
    build: !reset null
    environment:
      NOCTURNE_HTTP_TIMEOUT: 10s
      NOCTURNE_RECONCILIATION_INTERVAL: 3s
      NOCTURNE_WORKER_POLL_INTERVAL: 3s
      NOCTURNE_WORKER_LEASE_DURATION: 120s
      NOCTURNE_DELIVERY_TTL: 12s
      NOCTURNE_CANDIDATE_SWEEP_INTERVAL: 1s
      NOCTURNE_DELIVERY_SWEEP_INTERVAL: 1s
EOF

(
  cd "$ROOT/server"
  go test ./internal/integrations/nocturne -run 'TestManagedBackupRoundTripChunkBoundariesAndDestroyedRestore|TestManagedBackupErasureVerificationDestroyedArtifactSucceedsAndLiveKeyFails|TestManagedBackupPrecisePruneSuccess' -count=1
)

docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" --env-file "$ENV_FILE" -p "$PROJECT" up -d
rm -f "$GATE_LOG" "$COMPOSE_LOG"
set +e
python3 "$ROOT/contracttests/nocturne/compose_e2e.py" \
  --compose-file "$COMPOSE_FILE" \
  --override-file "$OVERRIDE_FILE" \
  --env-file "$ENV_FILE" \
  --project "$PROJECT" \
  --scenario "$SCENARIO" >"$GATE_LOG" 2>&1
GATE_RC=$?
set -e
cat "$GATE_LOG"
if [ "$GATE_RC" -ne 0 ]; then
  printf 'Nocturne Compose gate failed; preserved gate output at %s\n' "$GATE_LOG" >&2
  exit "$GATE_RC"
fi
