#!/usr/bin/env bash
set -Eeuo pipefail

IMAGE='haierkeys/fast-note-sync-service@sha256:15833f15e83cee05794c3fe6028c7e41fd36c787f0d651415cad556579fc379f'
EXPECTED_VERSION='3.6.1'
LOCK_FILE='/tmp/edu-agent-notesync-candidate.lock'
container=''

cleanup() {
	local exit_code=$?
	if [[ -n "$container" ]]; then
		docker rm -f "$container" >/dev/null 2>&1 || true
	fi
	if ((exit_code == 0)); then
		printf 'NoteSync candidate: PASS\n'
	else
		printf 'NoteSync candidate: FAIL\n' >&2
	fi
	trap - EXIT
	exit "$exit_code"
}
trap cleanup EXIT

for tool in docker curl jq go flock; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		printf 'NoteSync candidate: required tool not found: %s\n' "$tool" >&2
		exit 2
	fi
done
if ! docker info >/dev/null 2>&1; then
	printf 'NoteSync candidate: Docker daemon is unavailable\n' >&2
	exit 2
fi

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
	printf 'NoteSync candidate: another host-wide candidate gate holds %s\n' "$LOCK_FILE" >&2
	exit 2
fi

printf 'NoteSync candidate pinned image: %s\n' "$IMAGE"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	docker pull "$IMAGE" >/dev/null
fi

stamp="$(date -u +%Y%m%d%H%M%S)-$$"
container="edu-agent-notesync-candidate-$stamp"
docker run -d \
	--name "$container" \
	--tmpfs /fast-note-sync/storage:rw,nosuid,nodev,noexec,size=256m,mode=0777 \
	--tmpfs /fast-note-sync/config:rw,nosuid,nodev,noexec,size=16m,mode=0777 \
	-p 127.0.0.1::9000 \
	"$IMAGE" >/dev/null

binding="$(docker port "$container" 9000/tcp)"
port="${binding##*:}"
if [[ ! "$port" =~ ^[0-9]+$ ]]; then
	printf 'NoteSync candidate: Docker did not assign a localhost port\n' >&2
	exit 1
fi
base_url="http://127.0.0.1:$port"

deadline=$((SECONDS + 90))
ready=''
while ((SECONDS < deadline)); do
	health="$(curl -fsS --max-time 2 "$base_url/api/health" 2>/dev/null || true)"
	if jq -e '.status == true and .code == 1 and .data.status == "healthy" and .data.database == "connected"' <<<"$health" >/dev/null 2>&1; then
		ready='yes'
		break
	fi
	sleep 1
done
if [[ -z "$ready" ]]; then
	printf 'NoteSync candidate: service did not become healthy within 90 seconds\n' >&2
	docker logs --tail 80 "$container" >&2 || true
	exit 1
fi

version_response="$(curl -fsS --max-time 5 "$base_url/api/version")"
observed_version="$(jq -er 'select(.status == true and .code == 1) | .data.version | strings | select(length > 0)' <<<"$version_response")"
printf 'NoteSync candidate observed version: %s\n' "$observed_version"
if [[ "$observed_version" != "$EXPECTED_VERSION" ]]; then
	printf 'NoteSync candidate: expected version %s\n' "$EXPECTED_VERSION" >&2
	exit 1
fi

post_json() {
	local route=$1
	local auth_token=$2
	local payload=$3
	local response
	local -a args=(
		-fsS --max-time 10
		-H 'Accept: application/json'
		-H 'Content-Type: application/json'
		-H 'X-Client: webgui'
		-H 'User-Agent: edu-agent-notesync-candidate/1'
	)
	if [[ -n "$auth_token" ]]; then
		args+=(-H "Authorization: Bearer $auth_token")
	fi
	if ! response="$(curl "${args[@]}" --data "$payload" "$base_url$route")"; then
		printf 'NoteSync candidate: bootstrap request failed at %s\n' "$route" >&2
		return 1
	fi
	if ! jq -e '.status == true and (.code == 1 or .code == 2) and .data != null' <<<"$response" >/dev/null 2>&1; then
		business_code="$(jq -r '.code // "invalid-envelope"' <<<"$response" 2>/dev/null || printf '%s' 'invalid-envelope')"
		printf 'NoteSync candidate: bootstrap contract failed at %s (business code %s)\n' "$route" "$business_code" >&2
		return 1
	fi
	printf '%s' "$response"
}

email="notesync-candidate-$stamp@example.com"
username="c$(date -u +%s)${RANDOM}"
password="Candidate-$stamp-A9!"
vault="CandidateVault${stamp//-/}"

register_payload="$(jq -cn \
	--arg email "$email" --arg username "$username" --arg password "$password" \
	'{email:$email,username:$username,password:$password,confirmPassword:$password}')"
post_json '/api/user/register' '' "$register_payload" >/dev/null

login_payload="$(jq -cn --arg credentials "$email" --arg password "$password" \
	'{credentials:$credentials,password:$password,tokenId:0}')"
login_response="$(post_json '/api/user/login' '' "$login_payload")"
login_token="$(jq -er '.data.token | strings | select(length > 0)' <<<"$login_response")"

vault_payload="$(jq -cn --arg vault "$vault" '{vault:$vault,id:0}')"
post_json '/api/vault' "$login_token" "$vault_payload" >/dev/null

manual_token_payload="$(jq -cn --arg vault "$vault" \
	'{clientType:"CLI",scope:"",protocol:"rest",client:"CLI",function:"note_r,note_w",expiredDays:1,boundIp:"",userAgent:"",vaults:$vault}')"
manual_token_response="$(post_json '/api/token' "$login_token" "$manual_token_payload")"
api_token="$(jq -er '.data.token | strings | select(length >= 32)' <<<"$manual_token_response")"

printf '%s\n' 'NoteSync candidate scenarios: routes/auth/vault, create-only/readback/update/list, drift review/import/republish, response loss, outage/dependency, stale Outbox suppression'
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root/server"
NOTESYNC_REAL_BASE_URL="$base_url" \
NOTESYNC_REAL_API_TOKEN="$api_token" \
NOTESYNC_REAL_VAULT="$vault" \
	go test -count=1 -v \
	-run '^(TestRealUpstreamCandidate|TestPublicationConsumerCapabilityDependencyAndStaleSuppression)$' \
	./internal/integrations/notesync

cd "$repo_root"
NOTESYNC_REAL_BASE_URL="$base_url" \
NOTESYNC_REAL_API_TOKEN="$api_token" \
NOTESYNC_REAL_VAULT="$vault" \
	scripts/test-postgres-candidate.sh --shard db-core \
	--run '^TestPostgreSQLKnowledgeNotesyncRealUpstreamAcceptRemoteRepublishesWithoutLoop$'
