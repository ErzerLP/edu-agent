#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import subprocess
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any


class GateError(RuntimeError):
    pass


class Gate:
    def __init__(self, args: argparse.Namespace):
        self.compose_base = [
            "docker", "compose", "-f", args.compose_file, "-f", args.override_file,
            "--env-file", args.env_file, "-p", args.project,
        ]
        self.env = self._read_env(Path(args.env_file))
        self.base_url = f"http://127.0.0.1:{self.env['SERVER_PORT']}"
        self.token = ""
        self.device_id = ""

    @staticmethod
    def _read_env(path: Path) -> dict[str, str]:
        result: dict[str, str] = {}
        for line in path.read_text(encoding="utf-8").splitlines():
            if line and not line.startswith("#"):
                key, value = line.split("=", 1)
                result[key] = value
        return result

    def run(self, command: list[str], *, input_text: str | None = None, expected: int = 0) -> str:
        completed = subprocess.run(command, input=input_text, text=True, capture_output=True)
        if completed.returncode != expected:
            raise GateError(f"command failed without exposing captured output: {command[0]} {command[1] if len(command) > 1 else ''}")
        return completed.stdout.strip()

    def compose(self, *args: str, input_text: str | None = None, expected: int = 0) -> str:
        return self.run(self.compose_base + list(args), input_text=input_text, expected=expected)

    @staticmethod
    def decode_json(value: str, label: str) -> Any:
        try:
            return json.loads(value)
        except (TypeError, json.JSONDecodeError) as exc:
            raise GateError(f"{label} returned invalid JSON") from exc

    @staticmethod
    def decode_int(value: str, label: str) -> int:
        try:
            return int(value)
        except (TypeError, ValueError) as exc:
            raise GateError(f"{label} returned an invalid integer") from exc

    def wait_service_health(self, service: str, timeout: float = 120.0) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            container = self.compose("ps", "-q", service)
            if container:
                state = self.run(["docker", "inspect", "--format", "{{json .State.Health.Status}}", container])
                if self.decode_json(state, "container health") == "healthy":
                    return
            time.sleep(1)
        raise GateError(f"service did not become healthy: {service}")

    def http(self, method: str, path: str, body: Any | None = None, *, token: str | None = None) -> tuple[int, Any]:
        data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        request = urllib.request.Request(self.base_url + path, data=data, method=method)
        request.add_header("Accept", "application/json")
        if data is not None:
            request.add_header("Content-Type", "application/json")
        if token is not None:
            request.add_header("Authorization", f"Bearer {token}")
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                payload = response.read()
                return response.status, json.loads(payload) if payload else None
        except urllib.error.HTTPError as exc:
            payload = exc.read()
            return exc.code, json.loads(payload) if payload else None

    def wait_ready_status(self, expected: str, timeout: float = 60.0) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            try:
                status, body = self.http("GET", "/readyz")
                if status == 200 and isinstance(body, dict):
                    last = body
                    if body.get("status") == expected:
                        return body
            except OSError:
                pass
            time.sleep(1)
        raise GateError(f"readiness did not become {expected}: {last.get('status')}")

    def internal(self, method: str, path: str, *, token_mode: str, body: Any | None = None) -> tuple[int, Any]:
        program = r'''
import json, os, sys, urllib.error, urllib.request
request_data = json.load(sys.stdin)
token_mode = request_data["token_mode"]
token = ""
if token_mode == "api": token = os.environ["API_TOKEN"]
if token_mode == "maintenance": token = os.environ["EDU_AGENT_MAINTENANCE_TOKEN"]
data = None if request_data.get("body") is None else json.dumps(request_data["body"], separators=(",", ":")).encode()
request = urllib.request.Request("http://127.0.0.1:8233" + request_data["path"], data=data, method=request_data["method"])
request.add_header("Accept", "application/json")
request.add_header("X-Namespace", "edu-agent")
if data is not None: request.add_header("Content-Type", "application/json")
if token: request.add_header("Authorization", "Bearer " + token)
try:
    with urllib.request.urlopen(request, timeout=10) as response:
        payload = response.read()
        print(json.dumps({"status": response.status, "body": json.loads(payload) if payload else None}))
except urllib.error.HTTPError as exc:
    payload = exc.read()
    print(json.dumps({"status": exc.code, "body": json.loads(payload) if payload else None}))
'''
        payload = json.dumps({"method": method, "path": path, "token_mode": token_mode, "body": body})
        result = self.compose("exec", "-T", "nocturne", "python", "-c", program, input_text=payload)
        decoded = self.decode_json(result, "internal Nocturne request")
        if not isinstance(decoded, dict) or not isinstance(decoded.get("status"), int) or "body" not in decoded:
            raise GateError("internal Nocturne request returned an invalid envelope")
        return decoded["status"], decoded["body"]

    def sql(self, service: str, sql: str) -> str:
        script = 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "$1"'
        return self.compose("exec", "-T", service, "sh", "-eu", "-c", script, "sh", sql)

    def main_sql(self, sql: str) -> str:
        script = 'psql -v ON_ERROR_STOP=1 "$DATABASE_URL" -Atc "$1"'
        return self.compose("exec", "-T", "server", "sh", "-eu", "-c", script, "sh", sql)

    def pair(self) -> None:
        code = self.compose("run", "--rm", "--no-deps", "server", "pairing-code", "create")
        status, issued = self.http("POST", "/v1/pairings/exchange", {"code": code, "display_name": "compose-e2e"})
        if status != 201:
            raise GateError("pairing exchange failed")
        self.token = issued["token"]
        self.device_id = issued["device"]["id"]

    def create_memory(self, content: str) -> dict[str, Any]:
        request = {
            "operation_id": str(uuid.uuid4()), "payload_schema_version": 1,
            "content": content, "reason": "explicit compose gate preference",
            "category": "interaction_preference", "sensitivity": "non_sensitive", "stability": "stable",
            "valid_until": (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat().replace("+00:00", "Z"),
        }
        status, result = self.http("POST", "/v1/memory/candidates", request, token=self.token)
        if status not in (200, 201) or not isinstance(result, dict) or not result.get("record") or not result.get("delivery"):
            raise GateError("memory candidate was not durably admitted")
        return result

    def wait_memory_applied(self, logical_id: str, timeout: float = 60.0) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        last: dict[str, Any] = {}
        while time.monotonic() < deadline:
            status, body = self.http("GET", f"/v1/memory/records/{logical_id}", token=self.token)
            if status == 200 and isinstance(body, dict):
                last = body
                if body.get("delivery", {}).get("public_status") == "applied":
                    return body
            time.sleep(1)
        raise GateError(f"memory delivery did not apply: {last.get('delivery', {}).get('public_status')}")

    def check_auth_and_allowlist(self) -> None:
        for token_mode, expected in (("none", 401), ("api", 401), ("maintenance", 200)):
            status, body = self.internal("GET", "/internal/edu-agent/capabilities", token_mode=token_mode)
            if status != expected:
                raise GateError("maintenance capability authorization drifted")
            if expected == 200 and (body.get("upstream_commit") != "54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254" or body.get("compat_revision") != "edu-agent-maintenance-v1"):
                raise GateError("maintenance capability identity drifted")
        status, _ = self.internal("GET", "/api/browse/node", token_mode="maintenance")
        if status != 401:
            raise GateError("maintenance token crossed into the bridge API")
        for method, path in (("GET", "/sse"), ("POST", "/mcp"), ("GET", "/api/settings"), ("POST", "/api/browse/domains")):
            status, _ = self.internal(method, path, token_mode="api")
            if status != 404:
                raise GateError(f"route allowlist drifted: {path}")

    def check_real_nocturne_crud_and_absence(self) -> None:
        title = str(uuid.uuid4())
        path = f"edu-agent/{title}"
        create = {
            "parent_path": "edu-agent", "content": "compose history v1", "priority": 0,
            "disclosure": "compose gate", "title": title, "domain": "core",
        }
        status, created = self.internal("POST", "/api/browse/node", token_mode="api", body=create)
        if status != 200 or created.get("memory_id", 0) < 1:
            raise GateError("real Nocturne create failed")
        status, node = self.internal("GET", f"/api/browse/node?domain=core&path={path}", token_mode="api")
        if status != 200 or node.get("node", {}).get("content") != "compose history v1":
            raise GateError("real Nocturne read failed")
        node_id = node["node"]["node_uuid"]
        status, updated = self.internal("PUT", f"/api/browse/node?domain=core&path={path}", token_mode="api", body={"content": "compose history v2", "priority": 0, "disclosure": "compose gate"})
        if status != 200 or updated.get("memory_id", 0) == created["memory_id"]:
            raise GateError("real Nocturne history update failed")
        status, refs = self.internal("GET", f"/internal/edu-agent/nodes/{node_id}/references", token_mode="maintenance")
        memory_ids = refs.get("memory_ids", []) if status == 200 else []
        if len(memory_ids) < 2:
            raise GateError("Nocturne history was not enumerated")
        status, _ = self.internal("DELETE", f"/api/browse/node?domain=core&path={path}", token_mode="api")
        if status != 200:
            raise GateError("real Nocturne unlink failed")
        status, orphans = self.internal("GET", "/api/maintenance/orphans", token_mode="api")
        if status != 200 or not any(item.get("node_uuid") == node_id for item in orphans):
            raise GateError("global orphan enumeration missed the deleted node")
        for memory_id in sorted(memory_ids):
            status, _ = self.internal("DELETE", f"/api/maintenance/orphans/{memory_id}", token_mode="api")
            if status not in (200, 404):
                raise GateError("permanent history deletion failed")
        count_sql = f"""
SELECT
 (SELECT count(*) FROM nodes WHERE uuid='{node_id}')+
 (SELECT count(*) FROM memories WHERE node_uuid='{node_id}')+
 (SELECT count(*) FROM edges WHERE parent_uuid='{node_id}' OR child_uuid='{node_id}')+
 (SELECT count(*) FROM paths WHERE node_uuid='{node_id}')+
 (SELECT count(*) FROM glossary_keywords WHERE node_uuid='{node_id}')+
 (SELECT count(*) FROM search_documents WHERE node_uuid='{node_id}')+
 (SELECT count(*) FROM memory_access_logs WHERE node_uuid='{node_id}')
"""
        if self.sql("nocturne-postgres", count_sql).strip() != "0":
            raise GateError("Nocturne physical table absence check failed")
        if self.main_sql("SELECT (to_regclass('public.nodes') IS NULL)::int").strip() != "1":
            raise GateError("Nocturne tables leaked into the Go database")
        if self.sql("nocturne-postgres", "SELECT (to_regclass('public.memory_candidates') IS NULL)::int").strip() != "1":
            raise GateError("Go memory tables leaked into the Nocturne database")

    def check_database_account_isolation(self) -> None:
        main_to_nocturne = self.compose_base + [
            "exec", "-T", "-e", f"PGPASSWORD={self.env['POSTGRES_PASSWORD']}", "server", "psql",
            "-h", "nocturne-postgres", "-U", "edu_agent", "-d", "nocturne", "-Atc", "SELECT 1",
        ]
        nocturne_to_main = self.compose_base + [
            "exec", "-T", "-e", f"PGPASSWORD={self.env['NOCTURNE_POSTGRES_PASSWORD']}", "server", "psql",
            "-h", "postgres", "-U", "nocturne", "-d", "edu_agent", "-Atc", "SELECT 1",
        ]
        for command in (main_to_nocturne, nocturne_to_main):
            completed = subprocess.run(command, text=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if completed.returncode == 0:
                raise GateError("database accounts can read the other authority")

    def check_down_queue_and_replay(self) -> None:
        self.compose("stop", "nocturne")
        self.wait_ready_status("degraded")
        goal = {
            "operation_id": str(uuid.uuid4()), "payload_schema_version": 1, "aggregate_type": "goal",
            "aggregate_id": str(uuid.uuid4()), "expected_version": 0,
            "text": "Teaching remains available while Nocturne is down", "source": "device",
        }
        status, _ = self.http("POST", "/v1/learning/goals", goal, token=self.token)
        if status not in (200, 201):
            raise GateError("teaching write failed while Nocturne was down")
        queued = self.create_memory("I prefer concise summaries when services are unavailable")
        if queued["delivery"].get("public_status") != "queued":
            raise GateError("Nocturne outage did not preserve a queued delivery")
        logical_id = queued["record"]["logical_memory_id"]
        self.compose("start", "nocturne")
        self.wait_service_health("nocturne")
        ready = self.wait_ready_status("degraded")
        if ready.get("components", {}).get("nocturne", {}).get("status") != "healthy":
            raise GateError("Nocturne component did not recover to healthy")
        try:
            self.wait_memory_applied(logical_id, timeout=10)
        except GateError:
            delivery_id = queued["delivery"]["delivery_id"]
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                status, _ = self.http(
                    "POST", f"/v1/memory/deliveries/{delivery_id}/replays",
                    {"operation_id": str(uuid.uuid4()), "payload_schema_version": 1}, token=self.token,
                )
                if status in (200, 202):
                    break
                if status != 409:
                    raise GateError("dead memory delivery replay failed")
                time.sleep(2)
            else:
                raise GateError("memory delivery did not become replayable")
            try:
                self.wait_memory_applied(logical_id)
            except GateError as exc:
                state = self.main_sql(
                    "SELECT o.status||':'||o.attempts::text||':'||h.state||':'||"
                    "(h.lease_expires_at<=clock_timestamp())::text "
                    "FROM memory_deliveries d JOIN outbox_messages o ON o.id=d.outbox_id "
                    "LEFT JOIN memory_delivery_attempt_heads h ON h.delivery_id=d.id "
                    f"WHERE d.id='{delivery_id}'::uuid ORDER BY h.updated_at DESC LIMIT 1"
                )
                raise GateError(f"memory delivery replay did not converge: {state}") from exc

    def check_backup_encryption_key_destruction_and_prune(self) -> None:
        deadline = time.monotonic() + 45
        inventory: dict[str, Any] = {}
        while time.monotonic() < deadline:
            status, body = self.internal("GET", "/internal/edu-agent/backups", token_mode="maintenance")
            if status == 200 and len(body.get("artifacts", [])) >= 2:
                inventory = body
                break
            time.sleep(1)
        if not inventory:
            raise GateError("managed encrypted backup inventory did not populate")
        first = inventory["artifacts"][0]
        encrypted_check = 'file="/var/lib/edu-agent/nocturne-backups/$1"; [ "$(dd if="$file" bs=8 count=1 2>/dev/null)" = EDUMBKUP ]'
        self.compose("exec", "-T", "server", "sh", "-eu", "-c", encrypted_check, "sh", first["path"])
        if self.decode_int(self.main_sql("SELECT count(*) FROM memory_generation_keys WHERE destroyed_at IS NULL AND wrapped_key IS NOT NULL"), "live key count") < 1:
            raise GateError("managed backup key was not live before erasure")

        grant = self.compose("run", "--rm", "--no-deps", "server", "privacy-grant", "create", "--device", self.device_id)
        erasure_id = str(uuid.uuid4())
        request = urllib.request.Request(
            self.base_url + "/v1/privacy/erasures",
            data=json.dumps({
                "operation_id": erasure_id, "payload_schema_version": 1,
                "expected_current_learner_generation": 1, "reason_code": "learner_request", "explicit_confirmation": True,
            }).encode(),
            method="POST",
            headers={"Content-Type": "application/json", "Accept": "application/json", "Authorization": f"Bearer {self.token}", "X-Privacy-Erasure-Grant": grant},
        )
        with urllib.request.urlopen(request, timeout=30) as response:
            if response.status != 202:
                raise GateError("privacy erasure was not accepted")
            accepted = self.decode_json(response.read().decode("utf-8"), "privacy erasure response")
        receipt_id = accepted.get("erasure_id") if isinstance(accepted, dict) else None
        if not isinstance(receipt_id, str):
            raise GateError("privacy erasure response omitted the receipt ID")
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            status, receipt = self.http("GET", f"/v1/privacy/erasures/{receipt_id}", token=self.token)
            if status == 200 and receipt.get("status") == "verified":
                break
            time.sleep(2)
        else:
            raise GateError("privacy erasure did not verify managed backup destruction")
        if self.decode_int(self.main_sql("SELECT count(*) FROM memory_generation_keys WHERE learner_generation=1 AND destroyed_at IS NOT NULL AND wrapped_key IS NULL"), "destroyed key count") < 1:
            raise GateError("old generation wrapped key was not destroyed")

        old_path = first["path"]
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            status, body = self.internal("GET", "/internal/edu-agent/backups", token_mode="maintenance")
            if status == 200 and all(item["path"] != old_path for item in body.get("artifacts", [])):
                return
            time.sleep(2)
        raise GateError("retention prune did not remove the old encrypted artifact")

    def check_sigterm_shutdown(self) -> None:
        container = self.compose("ps", "-q", "server")
        self.compose("stop", "-t", "10", "server")
        exit_code = self.run(["docker", "inspect", "--format", "{{.State.ExitCode}}", container])
        if exit_code != "0":
            raise GateError("server did not exit cleanly after SIGTERM")

    def execute(self) -> None:
        for service in ("postgres", "nocturne-postgres", "nocturne", "server"):
            self.wait_service_health(service)
        ready = self.wait_ready_status("degraded")
        components = ready.get("components", {})
        if components.get("postgresql", {}).get("status") != "healthy" or components.get("nocturne", {}).get("status") != "healthy" or components.get("model", {}).get("reason") != "not_configured":
            raise GateError("initial readiness component state is invalid")
        self.pair()
        self.check_auth_and_allowlist()
        initial = self.create_memory("I prefer concise compose responses")
        self.wait_memory_applied(initial["record"]["logical_memory_id"])
        self.check_real_nocturne_crud_and_absence()
        self.check_database_account_isolation()
        self.check_down_queue_and_replay()
        self.check_backup_encryption_key_destruction_and_prune()
        self.check_sigterm_shutdown()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--compose-file", required=True)
    parser.add_argument("--override-file", required=True)
    parser.add_argument("--env-file", required=True)
    parser.add_argument("--project", required=True)
    return parser.parse_args()


if __name__ == "__main__":
    Gate(parse_args()).execute()
