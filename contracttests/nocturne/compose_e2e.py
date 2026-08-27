#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import time
import urllib.error
import urllib.parse
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
        self.compose_file = str(Path(args.compose_file).resolve())
        self.override_file = str(Path(args.override_file).resolve())
        self.env_file = str(Path(args.env_file).resolve())
        self.project = args.project
        self.scenario = args.scenario
        self.root = Path(self.compose_file).parents[1]
        self.temp_dir = Path(self.env_file).parent
        self.env = self._read_env(Path(self.env_file))
        self.image_lock = self.decode_json(
            (self.root / "deploy/nocturne/image.lock.json").read_text(encoding="utf-8"),
            "Nocturne image lock",
        )
        self.base_url = f"http://127.0.0.1:{self.env['SERVER_PORT']}"
        self.token = ""
        self.device_id = ""
        self.rollback_evidence: dict[str, str] = {}

    @staticmethod
    def _read_env(path: Path) -> dict[str, str]:
        result: dict[str, str] = {}
        for line in path.read_text(encoding="utf-8").splitlines():
            if line and not line.startswith("#"):
                key, value = line.split("=", 1)
                result[key] = value
        return result

    def run(
        self,
        command: list[str],
        *,
        input_text: str | None = None,
        expected: int = 0,
        environment: dict[str, str] | None = None,
    ) -> str:
        completed = subprocess.run(command, input=input_text, text=True, capture_output=True, env=environment)
        if completed.returncode != expected:
            raise GateError(f"command failed without exposing captured output: {command[0]} {command[1] if len(command) > 1 else ''}")
        return completed.stdout.strip()

    def expect_failure(self, command: list[str], label: str, *, environment: dict[str, str] | None = None) -> None:
        completed = subprocess.run(command, text=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=environment)
        if completed.returncode == 0:
            raise GateError(label)

    def run_rollback(self, command: list[str]) -> None:
        completed = subprocess.run(command, text=True, capture_output=True)
        if completed.returncode == 0:
            return
        reason = completed.stderr.strip().splitlines()[-1] if completed.stderr.strip() else "rollback command failed"
        if not reason.startswith("rollback failed:"):
            reason = "rollback command failed before validation"
        raise GateError(reason)

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

    @staticmethod
    def decode_float(value: str, label: str) -> float:
        try:
            return float(value)
        except (TypeError, ValueError) as exc:
            raise GateError(f"{label} returned an invalid number") from exc

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

    def internal(
        self,
        method: str,
        path: str,
        *,
        token_mode: str,
        body: Any | None = None,
        namespace: str = "edu-agent",
    ) -> tuple[int, Any]:
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
request.add_header("X-Namespace", request_data["namespace"])
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
        payload = json.dumps({
            "method": method, "path": path, "token_mode": token_mode, "body": body, "namespace": namespace,
        })
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

    @staticmethod
    def sql_text(value: str) -> str:
        return "'" + value.replace("'", "''") + "'"

    def wait_nocturne_search(
        self,
        query: str,
        path: str,
        *,
        namespace: str = "edu-agent",
        present: bool = True,
        timeout: float = 30.0,
    ) -> None:
        encoded = urllib.parse.urlencode({"q": query, "domain": "core", "limit": "100"})
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            status, body = self.internal(
                "GET", "/api/browse/search?" + encoded, token_mode="api", namespace=namespace,
            )
            found = status == 200 and isinstance(body, dict) and any(
                isinstance(item, dict) and item.get("path") == path for item in body.get("results", [])
            )
            if found == present:
                return
            time.sleep(0.5)
        state = "appear" if present else "disappear"
        raise GateError(f"Nocturne search result did not {state}")

    def create_memory(self, content: str, *, valid_for: timedelta = timedelta(hours=1)) -> dict[str, Any]:
        request = {
            "operation_id": str(uuid.uuid4()), "payload_schema_version": 1,
            "content": content, "reason": "explicit compose gate preference",
            "category": "interaction_preference", "sensitivity": "non_sensitive", "stability": "stable",
            "valid_until": (datetime.now(timezone.utc) + valid_for).isoformat().replace("+00:00", "Z"),
        }
        result: Any = None
        status: int | None = None
        for attempt in range(3):
            status, result = self.http("POST", "/v1/memory/candidates", request, token=self.token)
            if status in (200, 201):
                break
            if status != 500 or attempt == 2:
                raise GateError(f"memory candidate was not durably admitted: status={status} body={result}")
            time.sleep(1)
        if not isinstance(result, dict) or not result.get("record") or not result.get("delivery"):
            raise GateError(f"memory candidate was not durably admitted: status={status} body={result}")
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
        secondary_namespace = "zz-e2e-secondary"
        alias_path = f"alias/{title}"
        create = {
            "parent_path": "edu-agent", "content": "compose history v1", "priority": 0,
            "disclosure": "compose gate", "title": title, "domain": "core",
        }
        status, created = self.internal("POST", "/api/browse/node", token_mode="api", body=create)
        if status != 200 or not isinstance(created, dict) or created.get("memory_id", 0) < 1:
            raise GateError("real Nocturne create failed")
        query = urllib.parse.urlencode({"domain": "core", "path": path})
        status, node = self.internal("GET", "/api/browse/node?" + query, token_mode="api")
        if status != 200 or node.get("node", {}).get("content") != "compose history v1":
            raise GateError("real Nocturne read failed")
        node_id = node["node"]["node_uuid"]
        status, updated = self.internal(
            "PUT", "/api/browse/node?" + query, token_mode="api",
            body={"content": "compose history v2 searchable", "priority": 0, "disclosure": "compose gate"},
        )
        if status != 200 or updated.get("memory_id", 0) == created["memory_id"]:
            raise GateError("real Nocturne history update failed")
        self.wait_nocturne_search("searchable", path)

        fixture = f"""
WITH source_path AS (
  SELECT edge_id,node_uuid FROM paths
  WHERE namespace='edu-agent' AND domain='core' AND path={self.sql_text(path)}
), inserted_path AS (
  INSERT INTO paths(namespace,domain,path,edge_id,node_uuid,created_at)
  SELECT {self.sql_text(secondary_namespace)},'core',{self.sql_text(alias_path)},edge_id,node_uuid,clock_timestamp()
  FROM source_path RETURNING node_uuid
), source_search AS (
  SELECT node_uuid,memory_id,content,disclosure,search_terms,priority FROM search_documents
  WHERE namespace='edu-agent' AND domain='core' AND path={self.sql_text(path)}
), inserted_search AS (
  INSERT INTO search_documents(namespace,domain,path,node_uuid,memory_id,uri,content,disclosure,search_terms,priority,updated_at)
  SELECT {self.sql_text(secondary_namespace)},'core',{self.sql_text(alias_path)},node_uuid,memory_id,
         'core://'||{self.sql_text(alias_path)},content,disclosure,search_terms,priority,clock_timestamp()
  FROM source_search RETURNING node_uuid
), inserted_glossary AS (
  INSERT INTO glossary_keywords(keyword,node_uuid,namespace,created_at)
  VALUES({self.sql_text('compose-' + title)},'{node_id}',{self.sql_text(secondary_namespace)},clock_timestamp())
  RETURNING node_uuid
), inserted_log AS (
  INSERT INTO memory_access_logs(node_uuid,namespace,accessed_at,context)
  VALUES('{node_id}',{self.sql_text(secondary_namespace)},clock_timestamp(),'compose-e2e')
  RETURNING node_uuid
)
SELECT (SELECT count(*) FROM inserted_path)+(SELECT count(*) FROM inserted_search)+
       (SELECT count(*) FROM inserted_glossary)+(SELECT count(*) FROM inserted_log)
"""
        if self.sql("nocturne-postgres", fixture).strip() != "4":
            raise GateError("second namespace alias fixture was not installed")

        alias_query = urllib.parse.urlencode({"domain": "core", "path": alias_path})
        status, alias = self.internal(
            "GET", "/api/browse/node?" + alias_query, token_mode="api", namespace=secondary_namespace,
        )
        if status != 200 or alias.get("node", {}).get("node_uuid") != node_id:
            raise GateError("API token could not read the real second namespace alias")
        status, _ = self.internal(
            "GET", "/api/browse/node?" + alias_query, token_mode="maintenance", namespace=secondary_namespace,
        )
        if status != 401:
            raise GateError("namespace header crossed the bridge/maintenance authorization boundary")
        status, _ = self.internal(
            "GET", "/api/browse/node?" + query, token_mode="api", namespace=secondary_namespace,
        )
        if status != 404:
            raise GateError("namespace selection did not isolate browse routing")

        status, refs = self.internal(
            "GET", f"/internal/edu-agent/nodes/{node_id}/references",
            token_mode="maintenance", namespace=secondary_namespace,
        )
        memory_ids = refs.get("memory_ids", []) if status == 200 and isinstance(refs, dict) else []
        path_refs = {
            (item.get("namespace"), item.get("domain"), item.get("path"))
            for item in refs.get("paths", [])
        } if isinstance(refs, dict) else set()
        search_ids = set(refs.get("search_document_ids", [])) if isinstance(refs, dict) else set()
        if (
            len(memory_ids) < 2
            or ("edu-agent", "core", path) not in path_refs
            or (secondary_namespace, "core", alias_path) not in path_refs
            or f"edu-agent|core://{path}" not in search_ids
            or f"{secondary_namespace}|core://{alias_path}" not in search_ids
            or "compose-" + title not in refs.get("glossary_keywords", [])
            or not refs.get("access_log_ids")
        ):
            raise GateError("maintenance references were filtered by the namespace header")
        status, _ = self.internal(
            "GET", f"/internal/edu-agent/nodes/{node_id}/references?namespace={secondary_namespace}",
            token_mode="maintenance", namespace=secondary_namespace,
        )
        if status != 422:
            raise GateError("maintenance references accepted a namespace filter")
        self.wait_nocturne_search("searchable", alias_path, namespace=secondary_namespace)

        status, _ = self.internal("DELETE", "/api/browse/node?" + query, token_mode="api")
        if status != 200:
            raise GateError("primary Nocturne path unlink failed")
        status, alias = self.internal(
            "GET", "/api/browse/node?" + alias_query, token_mode="api", namespace=secondary_namespace,
        )
        if status != 200 or alias.get("node", {}).get("node_uuid") != node_id:
            raise GateError("secondary alias did not preserve the node after primary unlink")
        status, _ = self.internal(
            "DELETE", "/api/browse/node?" + alias_query, token_mode="api", namespace=secondary_namespace,
        )
        if status != 200:
            raise GateError("secondary Nocturne path unlink failed")
        status, orphans = self.internal(
            "GET", "/api/maintenance/orphans", token_mode="api", namespace=secondary_namespace,
        )
        if status != 200 or not any(item.get("node_uuid") == node_id for item in orphans):
            raise GateError("global orphan enumeration was filtered by namespace")
        for memory_id in sorted(memory_ids):
            status, _ = self.internal(
                "DELETE", f"/api/maintenance/orphans/{memory_id}",
                token_mode="api", namespace=secondary_namespace,
            )
            if status not in (200, 404):
                raise GateError("permanent history deletion failed")

        self.wait_nocturne_search("searchable", path, present=False)
        self.wait_nocturne_search("searchable", alias_path, namespace=secondary_namespace, present=False)
        status, _ = self.internal(
            "GET", f"/internal/edu-agent/nodes/{node_id}/references",
            token_mode="maintenance", namespace=secondary_namespace,
        )
        if status != 404:
            raise GateError("deleted node references remain addressable")
        status, orphans = self.internal(
            "GET", "/api/maintenance/orphans", token_mode="api", namespace=secondary_namespace,
        )
        if status != 200 or any(
            item.get("node_uuid") == node_id or item.get("memory_id") in memory_ids for item in orphans
        ):
            raise GateError("deleted node remains in global orphan enumeration")
        for memory_id in memory_ids:
            status, _ = self.internal(
                "GET", f"/api/maintenance/orphans/{memory_id}",
                token_mode="api", namespace=secondary_namespace,
            )
            if status != 404:
                raise GateError("known permanent memory ID remains addressable")

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

    def check_real_delivery_expiry_reconciliation(self) -> None:
        content = "I prefer concise compose expiry lost-response handling " + str(uuid.uuid4())
        self.compose("stop", "nocturne")
        self.wait_ready_status("degraded")
        queued = self.create_memory(content, valid_for=timedelta(seconds=12))
        if queued["delivery"].get("public_status") != "queued":
            raise GateError("expiry fixture did not create a queued delivery")
        logical_id = queued["record"]["logical_memory_id"]
        delivery_id = queued["delivery"]["delivery_id"]
        self.compose("stop", "-t", "10", "server")
        self.compose("start", "nocturne")
        self.wait_service_health("nocturne")

        status, capabilities = self.internal(
            "GET", "/internal/edu-agent/capabilities", token_mode="maintenance",
        )
        boot_epoch = capabilities.get("boot_epoch") if status == 200 and isinstance(capabilities, dict) else ""
        if not isinstance(boot_epoch, str) or not boot_epoch:
            raise GateError("expiry fixture could not record the real Nocturne boot epoch")
        create = {
            "parent_path": "edu-agent", "content": content, "priority": 0,
            "disclosure": "compose expiry reconciliation", "title": logical_id, "domain": "core",
        }
        status, remote_created = self.internal("POST", "/api/browse/node", token_mode="api", body=create)
        if status != 200 or not isinstance(remote_created, dict) or remote_created.get("memory_id", 0) < 1:
            raise GateError("expiry fixture remote write failed")
        path = "edu-agent/" + logical_id
        path_query = urllib.parse.urlencode({"domain": "core", "path": path})
        status, remote_node = self.internal("GET", "/api/browse/node?" + path_query, token_mode="api")
        node_id = remote_node.get("node", {}).get("node_uuid") if status == 200 else None
        if not isinstance(node_id, str) or remote_node["node"].get("content") != content:
            raise GateError("expiry fixture remote content was not durable")
        remote_memory_id = remote_created["memory_id"]

        attempt_id = str(uuid.uuid4())
        attempt_token = str(uuid.uuid4())
        lease_token = str(uuid.uuid4())
        fixture = f"""
WITH existing AS (
  SELECT h.current_attempt_id AS attempt_id,a.attempt_token
  FROM memory_delivery_heads h
  LEFT JOIN memory_delivery_attempts a ON a.id=h.current_attempt_id
  WHERE h.delivery_id='{delivery_id}'::uuid
), inserted_attempt AS (
  INSERT INTO memory_delivery_attempts(id,delivery_id,attempt_token,created_at)
  SELECT '{attempt_id}'::uuid,'{delivery_id}'::uuid,'{attempt_token}'::uuid,clock_timestamp()
  FROM existing WHERE attempt_id IS NULL
  RETURNING id AS attempt_id,attempt_token
), chosen AS (
  SELECT attempt_id,attempt_token FROM existing WHERE attempt_id IS NOT NULL
  UNION ALL SELECT attempt_id,attempt_token FROM inserted_attempt
), attempt_head AS (
  INSERT INTO memory_delivery_attempt_heads(
    attempt_id,delivery_id,state,lease_token,lease_expires_at,boot_epoch,sent_at,unknown_at,
    error_category,updated_at)
  SELECT attempt_id,'{delivery_id}'::uuid,'unknown','{lease_token}'::uuid,
         clock_timestamp()+interval '5 minutes',{self.sql_text(boot_epoch)},clock_timestamp(),clock_timestamp(),
         'compose_lost_response',clock_timestamp()
  FROM chosen
  ON CONFLICT(attempt_id) DO UPDATE SET
    state='unknown',lease_token=EXCLUDED.lease_token,lease_expires_at=EXCLUDED.lease_expires_at,
    boot_epoch=EXCLUDED.boot_epoch,sent_at=COALESCE(memory_delivery_attempt_heads.sent_at,EXCLUDED.sent_at),
    unknown_at=EXCLUDED.unknown_at,error_category=EXCLUDED.error_category,updated_at=EXCLUDED.updated_at
  RETURNING attempt_id,state,sent_at
), delivery_head AS (
  UPDATE memory_delivery_heads h
  SET current_attempt_id=a.attempt_id,attempt_state='unknown',
      attempt_count=CASE WHEN h.current_attempt_id IS NULL THEN h.attempt_count+1 ELSE h.attempt_count END,
      updated_at=clock_timestamp()
  FROM attempt_head a WHERE h.delivery_id='{delivery_id}'::uuid
  RETURNING h.delivery_id,h.attempt_state
), outbox_update AS (
  UPDATE outbox_messages o
  SET status='pending',terminal_disposition=NULL,available_at=d.valid_until+interval '5 minutes',
      lease_token=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
  FROM memory_deliveries d
  WHERE d.id='{delivery_id}'::uuid AND o.id=d.outbox_id
  RETURNING o.status
)
SELECT json_build_object(
  'attempt_state',(SELECT state FROM attempt_head),
  'was_sent',(SELECT sent_at IS NOT NULL FROM attempt_head),
  'delivery_state',(SELECT attempt_state FROM delivery_head),
  'outbox_state',(SELECT status FROM outbox_update)
)::text
"""
        fixture_state = self.decode_json(self.sql("postgres", fixture), "expiry delivery fixture")
        if fixture_state != {
            "attempt_state": "unknown", "was_sent": True,
            "delivery_state": "unknown", "outbox_state": "pending",
        }:
            raise GateError("expiry delivery fixture was not a legal sent/unknown attempt")

        remaining = self.decode_float(
            self.sql(
                "postgres",
                f"SELECT GREATEST(0,EXTRACT(EPOCH FROM (valid_until-clock_timestamp()))) FROM memory_deliveries WHERE id='{delivery_id}'::uuid",
            ),
            "delivery expiry delay",
        )
        if remaining > 0:
            time.sleep(remaining + 0.5)
        self.compose("start", "server")
        self.wait_service_health("server")
        self.wait_ready_status("degraded")

        # Cover one expired claim plus one complete remote reconciliation under the fixed 120s lease.
        deadline = time.monotonic() + 270
        terminal: dict[str, Any] = {}
        while time.monotonic() < deadline:
            state_sql = f"""
SELECT json_build_object(
  'delivery',h.status,'outbox',o.status,'outbox_payload',o.payload,
  'record',rh.status,
  'delivery_payloads',(SELECT count(*) FROM memory_delivery_payloads p WHERE p.delivery_id=d.id),
  'reconciliation_count',(SELECT count(*) FROM memory_expiry_reconciliations r WHERE r.delivery_id=d.id),
  'verified_reconciliations',(SELECT count(*) FROM memory_expiry_reconciliations r WHERE r.delivery_id=d.id AND r.status='verified'),
  'remote_delete_plans',(SELECT count(*) FROM memory_remote_delete_plans p WHERE p.delivery_id=d.id)
)::text
FROM memory_deliveries d
JOIN memory_delivery_heads h ON h.delivery_id=d.id
JOIN memory_record_heads rh ON rh.current_delivery_id=d.id
JOIN outbox_messages o ON o.id=d.outbox_id
WHERE d.id='{delivery_id}'::uuid
"""
            terminal = self.decode_json(self.main_sql(state_sql), "expiry reconciliation state")
            if (
                terminal.get("delivery") == "expired"
                and terminal.get("outbox") == "canceled"
                and terminal.get("record") == "permanently_rejected"
                and terminal.get("delivery_payloads") == 0
                and terminal.get("reconciliation_count") == 1
                and terminal.get("verified_reconciliations") == 1
                and terminal.get("remote_delete_plans") == 1
            ):
                break
            time.sleep(1)
        else:
            raise GateError(f"real delivery expiry reconciliation did not converge: {terminal}")
        outbox_payload = terminal.get("outbox_payload")
        if not isinstance(outbox_payload, dict) or set(outbox_payload) != {
            "delivery_id", "payload_hash", "record_revision", "learner_generation", "record_generation",
        }:
            raise GateError("expired outbox payload was not the frozen body-free intent")

        plaintext_sql = f"""
SELECT
 (SELECT count(*) FROM memory_candidate_payloads p
    JOIN memory_record_revisions r ON r.candidate_id=p.candidate_id
    JOIN memory_deliveries d ON d.record_revision_id=r.id
    WHERE d.id='{delivery_id}'::uuid AND p.content={self.sql_text(content)})+
 (SELECT count(*) FROM memory_delivery_payloads p
    WHERE p.delivery_id='{delivery_id}'::uuid AND p.content={self.sql_text(content)})+
 (SELECT count(*) FROM outbox_messages o
    JOIN memory_deliveries d ON d.outbox_id=o.id
    WHERE d.id='{delivery_id}'::uuid AND position({self.sql_text(content)} in o.payload::text)>0)+
 (SELECT count(*) FROM memory_expiry_reconciliations r
    WHERE r.delivery_id='{delivery_id}'::uuid AND position({self.sql_text(content)} in row_to_json(r)::text)>0)
"""
        if self.main_sql(plaintext_sql).strip() != "0":
            raise GateError("expiry reconciliation retained plaintext")
        if self.main_sql(
            "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' "
            "AND table_name='memory_expiry_reconciliations' AND column_name IN ('content','payload','body')"
        ).strip() != "0":
            raise GateError("expiry reconciliation schema contains a plaintext column")

        status, _ = self.internal("GET", "/api/browse/node?" + path_query, token_mode="api")
        if status != 404:
            raise GateError("Purger left the expiry-reconciled path readable")
        self.wait_nocturne_search(logical_id, path, present=False)
        status, _ = self.internal(
            "GET", f"/internal/edu-agent/nodes/{node_id}/references", token_mode="maintenance",
        )
        if status != 404:
            raise GateError("Purger left expiry-reconciled references")
        status, orphans = self.internal("GET", "/api/maintenance/orphans", token_mode="api")
        if status != 200 or any(
            item.get("node_uuid") == node_id or item.get("memory_id") == remote_memory_id for item in orphans
        ):
            raise GateError("Purger left an expiry-reconciled global orphan")
        status, _ = self.internal("GET", f"/api/maintenance/orphans/{remote_memory_id}", token_mode="api")
        if status != 404:
            raise GateError("Purger left the known expiry-reconciled memory ID")

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

    def delivery_runtime_state(self, delivery_id: str) -> str:
        return self.main_sql(
            "SELECT concat_ws(':',o.status,o.attempts::text,COALESCE(o.last_error_category,'none'),"
            "COALESCE(h.state,'none'),COALESCE((h.sent_at IS NOT NULL)::text,'none'),"
            "COALESCE((h.lease_expires_at<=clock_timestamp())::text,'none')) "
            "FROM memory_deliveries d JOIN outbox_messages o ON o.id=d.outbox_id "
            "LEFT JOIN memory_delivery_attempt_heads h ON h.delivery_id=d.id "
            f"WHERE d.id='{delivery_id}'::uuid ORDER BY h.updated_at DESC LIMIT 1"
        )

    def check_down_queue_auto_recovery(self) -> None:
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
        queued = self.create_memory("I prefer concise summaries after automatic recovery")
        if queued["delivery"].get("public_status") != "queued":
            raise GateError("Nocturne outage did not preserve a queued delivery")
        logical_id = queued["record"]["logical_memory_id"]
        delivery_id = queued["delivery"]["delivery_id"]
        self.compose("start", "nocturne")
        self.wait_service_health("nocturne")
        ready = self.wait_ready_status("degraded")
        if ready.get("components", {}).get("nocturne", {}).get("status") != "healthy":
            raise GateError("Nocturne component did not recover to healthy")
        try:
            self.wait_memory_applied(logical_id, timeout=60)
        except GateError as exc:
            raise GateError(
                f"memory delivery did not recover automatically: {self.delivery_runtime_state(delivery_id)}"
            ) from exc

    def check_dead_delivery_replay(self) -> None:
        self.sql("nocturne-postgres", "ALTER TABLE public.nodes RENAME TO nodes_dead_letter_gate")
        fault_injected = True
        failure: Exception | None = None
        logical_id = ""
        delivery_id = ""
        try:
            self.wait_service_health("nocturne")
            queued = self.create_memory("I prefer concise retry explanations")
            if queued["delivery"].get("public_status") != "queued":
                raise GateError("Nocturne mutation failure did not preserve the replay candidate")
            logical_id = queued["record"]["logical_memory_id"]
            delivery_id = queued["delivery"]["delivery_id"]
            status, response = self.http(
                "POST", f"/v1/memory/deliveries/{delivery_id}/replays",
                {"operation_id": str(uuid.uuid4()), "payload_schema_version": 1}, token=self.token,
            )
            if status != 409:
                raise GateError(f"pending memory delivery replay status={status}, want 409: {response}")
            deadline = time.monotonic() + 90
            state = self.delivery_runtime_state(delivery_id)
            while time.monotonic() < deadline and not state.startswith("dead:"):
                time.sleep(2)
                state = self.delivery_runtime_state(delivery_id)
            parts = state.split(":")
            if (
                len(parts) < 6
                or parts[0] != "dead"
                or parts[1] != "5"
                or parts[2] != "consumer_error"
                or parts[3] not in ("unknown", "reconciling")
                or parts[4] != "true"
            ):
                raise GateError(f"memory delivery did not exhaust transient retries: {state}")

            status, response = self.http(
                "POST", f"/v1/memory/deliveries/{delivery_id}/replays",
                {"operation_id": str(uuid.uuid4()), "payload_schema_version": 1}, token=self.token,
            )
            if status != 202:
                raise GateError(f"dead memory delivery replay status={status}, want 202: {response}")

            self.sql("nocturne-postgres", "ALTER TABLE public.nodes_dead_letter_gate RENAME TO nodes")
            fault_injected = False
            self.compose("restart", "nocturne")
            self.wait_service_health("nocturne")
            ready = self.wait_ready_status("degraded")
            if ready.get("components", {}).get("nocturne", {}).get("status") != "healthy":
                raise GateError("Nocturne component did not restart after replay")
            try:
                self.wait_memory_applied(logical_id, timeout=60)
            except GateError as exc:
                raise GateError(
                    f"memory delivery replay did not converge: {self.delivery_runtime_state(delivery_id)}"
                ) from exc
        except Exception as exc:
            failure = exc
        finally:
            if fault_injected:
                try:
                    self.sql("nocturne-postgres", "ALTER TABLE public.nodes_dead_letter_gate RENAME TO nodes")
                except Exception as restore_exc:
                    if failure is not None:
                        raise GateError(f"{failure}; failed to restore Nocturne nodes table: {restore_exc}") from failure
                    raise
        if failure is not None:
            raise failure

    def original_nocturne_volume(self) -> str:
        container = self.compose("ps", "-q", "nocturne-postgres")
        inspected = self.decode_json(
            self.run(["docker", "inspect", container]), "original Nocturne database container",
        )
        try:
            return next(
                item["Name"] for item in inspected[0]["Mounts"]
                if item.get("Destination") == "/var/lib/postgresql/data" and item.get("Type") == "volume"
            )
        except (IndexError, KeyError, StopIteration, TypeError) as exc:
            raise GateError("original Nocturne database volume was not identifiable") from exc

    def rollback_command(
        self,
        *,
        record: Path,
        target_volume: str,
        target_snapshot_volume: str,
        old_image: str | None = None,
    ) -> list[str]:
        evidence = self.rollback_evidence
        return [
            "sh", str(self.root / "deploy/nocturne/scripts/rollback.sh"),
            "--compose-file", self.compose_file,
            "--env-file", self.env_file,
            "--project", self.project,
            "--artifact", evidence["artifact"],
            "--old-image", old_image or self.env["NOCTURNE_IMAGE"],
            "--expected-platform", self.image_lock["platform"],
            "--expected-config-digest", self.image_lock["config_digest"],
            "--expected-upstream-commit", "54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254",
            "--expected-compat-revision", "edu-agent-maintenance-v1",
            "--expected-schema-version", evidence["schema_version"],
            "--target-volume", target_volume,
            "--target-snapshot-volume", target_snapshot_volume,
            "--seed-path", evidence["seed_path"],
            "--seed-search-query", evidence["seed_search_query"],
            "--seed-content-sha256", evidence["seed_content_sha256"],
            "--record", str(record),
        ]

    def wait_new_backup(self, previous_paths: set[str], timeout: float = 45.0) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            status, body = self.internal("GET", "/internal/edu-agent/backups", token_mode="maintenance")
            if status == 200 and isinstance(body, dict):
                fresh = [
                    item for item in body.get("artifacts", [])
                    if isinstance(item, dict) and item.get("path") not in previous_paths
                    and item.get("learner_generation") == 1
                ]
                if len(fresh) >= 2:
                    for item in sorted(fresh, key=lambda value: value["created_at"], reverse=True):
                        path = item.get("path")
                        if not isinstance(path, str):
                            continue
                        durable = self.main_sql(
                            "SELECT count(*) FROM memory_managed_backup_inventory i "
                            "JOIN memory_generation_keys k ON k.id=i.wrapped_key_id AND k.learner_generation=i.learner_generation "
                            f"WHERE i.relative_path={self.sql_text(path)} AND i.pruned_at IS NULL "
                            "AND k.destroyed_at IS NULL AND k.wrapped_key IS NOT NULL"
                        ).strip()
                        exists = subprocess.run(
                            self.compose_base + [
                                "exec", "-T", "server", "test", "-f",
                                "/var/lib/edu-agent/nocturne-backups/" + path,
                            ],
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL,
                        )
                        if durable == "1" and exists.returncode == 0:
                            return item
            time.sleep(1)
        raise GateError("a post-seed managed backup was not produced")

    def apply_failed_forward_upgrade(self, seed_path: str) -> None:
        forward_image = self.env["NOCTURNE_FAILED_FORWARD_IMAGE"]
        forward_config = self.env["NOCTURNE_FAILED_FORWARD_CONFIG_DIGEST"]
        fixture_digest = self.env["NOCTURNE_FAILED_FORWARD_FIXTURE_SHA256"]
        old_digest = self.env["NOCTURNE_IMAGE"].split("@", 1)[1]
        forward_digest = forward_image.split("@", 1)[1]
        if forward_digest == old_digest or not re.fullmatch(r"sha256:[0-9a-f]{64}", forward_digest):
            raise GateError("failed-forward release is not independently digest pinned")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", forward_config) or not re.fullmatch(r"[0-9a-f]{64}", fixture_digest):
            raise GateError("failed-forward release metadata is invalid")

        self.compose("stop", "server", "nocturne")
        database_url = (
            "postgresql+asyncpg://"
            + self.env["NOCTURNE_POSTGRES_USER"] + ":" + self.env["NOCTURNE_POSTGRES_PASSWORD"]
            + "@nocturne-postgres:5432/" + self.env["NOCTURNE_POSTGRES_DB"] + "?ssl=disable"
        )
        self.run([
            "docker", "run", "--rm", "--network", self.project + "_nocturne-internal",
            "-e", "DATABASE_URL=" + database_url,
            "-e", "EDU_AGENT_FAILED_FORWARD_FIXTURE_SHA256=" + fixture_digest,
            "-e", "EDU_AGENT_FAILED_FORWARD_BASE_DIGEST=" + old_digest,
            forward_image,
        ], expected=42)
        marker = self.sql("nocturne-postgres", """
            SELECT concat_ws('|',
                COALESCE(to_regclass('public.nodes')::text,''),
                COALESCE(to_regclass('public.nodes_pre_a84_failed_forward')::text,''),
                COALESCE((SELECT fixture_sha256 FROM edu_agent_failed_forward_release WHERE singleton),''),
                COALESCE((SELECT base_platform_digest FROM edu_agent_failed_forward_release WHERE singleton),''))
        """).split("|")
        if marker != ["", "nodes_pre_a84_failed_forward", fixture_digest, old_digest]:
            raise GateError("failed-forward schema mutation evidence is incomplete")

        self.compose("up", "-d", "nocturne")
        old_image_accepted_forward_schema = False
        try:
            self.wait_service_health("nocturne", timeout=45.0)
            query = urllib.parse.urlencode({"domain": "core", "path": seed_path})
            status, body = self.internal("GET", "/api/browse/node?" + query, token_mode="api")
            content = body.get("node", {}).get("content", "") if isinstance(body, dict) else ""
            old_image_accepted_forward_schema = (
                status == 200
                and hashlib.sha256(content.encode("utf-8")).hexdigest() == self.rollback_evidence["seed_content_sha256"]
            )
        except GateError:
            pass
        if old_image_accepted_forward_schema:
            raise GateError("old Nocturne image accepted the failed-forward database")
        self.compose("stop", "nocturne")
        self.rollback_evidence.update({
            "failed_forward_image": forward_image,
            "failed_forward_config_digest": forward_config,
            "failed_forward_fixture_sha256": fixture_digest,
        })

    def restore_original_nocturne_database(self, artifact: str) -> None:
        self.compose("up", "-d", "nocturne-postgres")
        self.wait_service_health("nocturne-postgres")
        script = (
            'artifact=$1; output=/dev/shm/edu-agent-original-restore.dump; '
            'edu-agentd nocturne-backup restore --artifact "$artifact" --output "$output"; '
            'psql "$NOCTURNE_PG_DUMP_DSN" -v ON_ERROR_STOP=1 '
            "-c 'DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public; DROP SCHEMA IF EXISTS edu_agent_maintenance CASCADE;'; "
            'pg_restore --exit-on-error --no-owner --no-privileges --dbname="$NOCTURNE_PG_DUMP_DSN" "$output"; '
            'rm -f "$output"'
        )
        self.run(self.compose_base + [
            "run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "server",
            "-eu", "-c", script, "restore-original", artifact,
        ])
        restored = self.sql("nocturne-postgres", """
            SELECT concat_ws('|',
                COALESCE(to_regclass('public.nodes')::text,''),
                COALESCE(to_regclass('public.nodes_pre_a84_failed_forward')::text,''),
                COALESCE(to_regclass('public.edu_agent_failed_forward_release')::text,''))
        """).split("|")
        if restored != ["nodes", "", ""]:
            raise GateError("pre-upgrade database was not restored after the rollback rehearsal")

    def check_real_rollback_rehearsal(self) -> None:
        status, inventory = self.internal("GET", "/internal/edu-agent/backups", token_mode="maintenance")
        if status != 200 or not isinstance(inventory, dict):
            raise GateError("rollback pre-seed backup inventory is unavailable")
        previous_paths = {item["path"] for item in inventory.get("artifacts", [])}
        parent_query = urllib.parse.urlencode({"domain": "core", "path": "edu-agent"})
        parent_status, parent_response = self.internal(
            "GET", "/api/browse/node?" + parent_query, token_mode="api",
        )
        if parent_status == 404:
            parent_status, parent_response = self.internal(
                "POST", "/api/browse/node", token_mode="api", body={
                    "parent_path": "", "content": "edu-agent rollback rehearsal root",
                    "priority": 0, "disclosure": "rollback rehearsal root",
                    "title": "edu-agent", "domain": "core",
                },
            )
        if parent_status != 200:
            raise GateError(
                f"rollback seed parent preparation failed ({parent_status}): {parent_response}"
            )
        title = str(uuid.uuid4())
        seed_path = "edu-agent/" + title
        seed_content = "same-digest rollback seed " + str(uuid.uuid4())
        status, response = self.internal("POST", "/api/browse/node", token_mode="api", body={
            "parent_path": "edu-agent", "content": seed_content, "priority": 0,
            "disclosure": "rollback rehearsal seed", "title": title, "domain": "core",
        })
        if status != 200:
            raise GateError(f"rollback seed creation failed ({status}): {response}")
        self.wait_nocturne_search("rollback seed", seed_path)
        artifact = self.wait_new_backup(previous_paths)
        self.rollback_evidence = {
            "artifact": artifact["path"],
            "schema_version": self.sql("nocturne-postgres", "SELECT COALESCE(max(version),'') FROM schema_migrations").strip(),
            "seed_path": seed_path,
            "seed_search_query": "rollback seed",
            "seed_content_sha256": hashlib.sha256(seed_content.encode("utf-8")).hexdigest(),
        }
        if not self.rollback_evidence["schema_version"]:
            raise GateError("rollback schema evidence is unavailable")

        original_volume = self.original_nocturne_volume()
        prefix = self.project[:80]
        floating_record = self.temp_dir / "rollback-floating.json"
        repository = self.env["NOCTURNE_IMAGE"].split("@", 1)[0]
        self.expect_failure(
            self.rollback_command(
                record=floating_record,
                target_volume=prefix + "-floating-db",
                target_snapshot_volume=prefix + "-floating-snap",
                old_image=repository + ":floating",
            ),
            "rollback accepted a floating image",
        )

        original_record = self.temp_dir / "rollback-original-volume.json"
        self.expect_failure(
            self.rollback_command(
                record=original_record,
                target_volume=original_volume,
                target_snapshot_volume=prefix + "-original-snap",
            ),
            "rollback accepted the original database volume",
        )

        nonempty_volume = prefix + "-nonempty-db"
        nonempty_snapshot = prefix + "-nonempty-snap"
        self.run(["docker", "volume", "create", nonempty_volume])
        try:
            self.run([
                "docker", "run", "--rm", "-v", nonempty_volume + ":/target",
                "--entrypoint", "sh",
                "postgres:15-alpine@sha256:fe0737ba566a2c5b2a28f34433c0a423261900ec17b9bf7ad115e1aae7e57f1b",
                "-eu", "-c", "printf nonempty >/target/sentinel",
            ])
            self.expect_failure(
                self.rollback_command(
                    record=self.temp_dir / "rollback-nonempty.json",
                    target_volume=nonempty_volume,
                    target_snapshot_volume=nonempty_snapshot,
                ),
                "rollback accepted a pre-existing non-empty target",
            )
        finally:
            self.run(["docker", "volume", "rm", nonempty_volume])

        self.apply_failed_forward_upgrade(seed_path)
        target_volume = prefix + "-validated-db"
        target_snapshot = prefix + "-validated-snap"
        record_path = self.temp_dir / "rollback-validated.json"
        try:
            self.run_rollback(self.rollback_command(
                record=record_path,
                target_volume=target_volume,
                target_snapshot_volume=target_snapshot,
            ))
        except GateError as exc:
            if record_path.is_file():
                failed = self.decode_json(record_path.read_text(encoding="utf-8"), "failed rollback record")
                raise GateError(f"rollback rehearsal failed: {failed.get('failure_reason', 'unknown')}") from exc
            raise
        record = self.decode_json(record_path.read_text(encoding="utf-8"), "rollback validation record")
        if (
            record.get("status") != "validated"
            or record.get("target_database_volume") != target_volume
            or record.get("original_database_volume") != original_volume
            or record.get("config_digest") != self.image_lock["config_digest"]
            or record.get("old_image") != self.env["NOCTURNE_IMAGE"]
            or record.get("old_image") == self.rollback_evidence["failed_forward_image"]
            or record.get("bridge_writer") != "stopped-pending-operator-release"
        ):
            raise GateError("rollback validation record is incomplete")

        override = Path(record["compose_override"])
        rollback_compose = [
            "docker", "compose", "-f", self.compose_file, "-f", str(override),
            "--env-file", self.env_file, "-p", self.project,
        ]
        rollback_environment = dict(os.environ, NOCTURNE_ROLLBACK_IMAGE=self.env["NOCTURNE_IMAGE"])
        container = self.run(
            rollback_compose + ["ps", "-q", "nocturne"], environment=rollback_environment,
        )
        running_manifest = self.run(["docker", "inspect", "--format", "{{.Image}}", container])
        if running_manifest != self.image_lock["platform_manifest_digest"]:
            raise GateError("rollback Nocturne did not run the locked old platform manifest")
        rollback_db = self.run(
            rollback_compose + ["ps", "-q", "nocturne-rollback-postgres"],
            environment=rollback_environment,
        )
        mounts = self.decode_json(
            self.run(["docker", "inspect", "--format", "{{json .Mounts}}", rollback_db]),
            "rollback database mounts",
        )
        if not any(
            item.get("Destination") == "/var/lib/postgresql/data"
            and item.get("Name") == target_volume
            and item.get("Name") != original_volume
            for item in mounts
        ):
            raise GateError("rollback database was not mounted from the new volume")

        self.run(
            rollback_compose + ["stop", "nocturne", "nocturne-rollback-postgres"],
            environment=rollback_environment,
        )
        self.run(
            rollback_compose + ["rm", "-f", "nocturne", "nocturne-rollback-postgres"],
            environment=rollback_environment,
        )
        self.run(["docker", "volume", "rm", target_volume, target_snapshot])
        self.restore_original_nocturne_database(self.rollback_evidence["artifact"])
        self.compose("up", "-d", "nocturne", "server")
        for service in ("nocturne-postgres", "nocturne", "server"):
            self.wait_service_health(service)
        self.wait_ready_status("degraded")
        seed_query = urllib.parse.urlencode({"domain": "core", "path": seed_path})
        status, seed = self.internal("GET", "/api/browse/node?" + seed_query, token_mode="api")
        if status != 200 or hashlib.sha256(seed.get("node", {}).get("content", "").encode()).hexdigest() != self.rollback_evidence["seed_content_sha256"]:
            raise GateError("original Nocturne volume did not survive rollback rehearsal")

    def restore_artifact_command(self, artifact: str) -> list[str]:
        script = (
            'artifact=$1; output=/dev/shm/edu-agent-restore.dump; '
            'edu-agentd nocturne-backup restore --artifact "$artifact" --output "$output"; '
            '[ "$(stat -c %a "$output")" = 600 ]; '
            '[ "$(dd if="$output" bs=5 count=1 2>/dev/null)" = PGDMP ]; rm -f "$output"'
        )
        return self.compose_base + [
            "run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "server",
            "-eu", "-c", script, "restore-check", artifact,
        ]

    def check_destroyed_key_rollback_failure(self) -> None:
        prefix = self.project[:80]
        target_volume = prefix + "-destroyed-db"
        target_snapshot = prefix + "-destroyed-snap"
        record_path = self.temp_dir / "rollback-destroyed-key.json"
        self.expect_failure(
            self.rollback_command(
                record=record_path,
                target_volume=target_volume,
                target_snapshot_volume=target_snapshot,
            ),
            "rollback accepted an artifact with a destroyed generation key",
        )
        record = self.decode_json(record_path.read_text(encoding="utf-8"), "destroyed-key rollback record")
        if record.get("status") != "failed-isolated" or "recoverable" not in record.get("failure_reason", ""):
            raise GateError("destroyed-key rollback did not fail in restore preflight")
        for volume in (target_volume, target_snapshot):
            completed = subprocess.run(
                ["docker", "volume", "inspect", volume],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            if completed.returncode == 0:
                raise GateError("destroyed-key rollback created a target volume")
        self.compose("up", "-d", "nocturne", "server")
        for service in ("nocturne", "server"):
            self.wait_service_health(service)
        self.wait_ready_status("degraded")

    def check_backup_encryption_key_destruction_and_prune(self) -> None:
        if not self.rollback_evidence:
            raise GateError("rollback artifact evidence is unavailable")
        old_path = self.rollback_evidence["artifact"]
        encrypted_check = 'file="/var/lib/edu-agent/nocturne-backups/$1"; [ "$(dd if="$file" bs=8 count=1 2>/dev/null)" = EDUMBKUP ]'
        self.compose("exec", "-T", "server", "sh", "-eu", "-c", encrypted_check, "sh", old_path)
        if self.decode_int(self.main_sql("SELECT count(*) FROM memory_generation_keys WHERE destroyed_at IS NULL AND wrapped_key IS NOT NULL"), "live key count") < 1:
            raise GateError("managed backup key was not live before erasure")
        self.run(self.restore_artifact_command(old_path))

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
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                if response.status != 202:
                    raise GateError("privacy erasure was not accepted")
                accepted = self.decode_json(response.read().decode("utf-8"), "privacy erasure response")
        except urllib.error.HTTPError as exc:
            payload = self.decode_json(exc.read().decode("utf-8"), "privacy erasure error")
            state = self.sql(
                "postgres",
                "SELECT json_build_object('generation',(SELECT learner_generation FROM privacy_owner_generation_gates WHERE owner_kind='memory'),"
                "'active_erasure',(SELECT e.id FROM privacy_erasures e JOIN privacy_erasure_heads h ON h.erasure_id=e.id WHERE h.status IN ('barrier_committed','local_scrubbed','remote_draining','remote_purged','partial','blocked') LIMIT 1),"
                "'migration_lease',(SELECT operation_id FROM privacy_migration_lease WHERE singleton_id=1 AND operation_id IS NOT NULL AND released_at IS NULL))::text",
            )
            raise GateError(f"privacy erasure was not accepted: status={exc.code} body={payload} state={state}") from exc
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
            receipt_state = self.main_sql(
                "SELECT json_build_object("
                "'erasure',(SELECT status FROM privacy_erasure_heads WHERE erasure_id=$$" + receipt_id + "$$::uuid),"
                "'stores',(SELECT json_agg(json_build_object('store',r.store_kind,'status',r.status,'reason',r.stable_reason) ORDER BY r.store_kind) "
                "FROM privacy_erasure_receipt_heads rh JOIN privacy_erasure_step_receipts r ON r.id=rh.current_receipt_id WHERE rh.erasure_id=$$" + receipt_id + "$$::uuid),"
                "'reconciliations',(SELECT COALESCE(json_agg(json_build_object('status',status,'lease',lease_expires_at IS NOT NULL)), '[]'::json) FROM memory_expiry_reconciliations),"
                "'erasure_scopes',(SELECT COALESCE(json_agg(json_build_object('store',s.store_kind,'status',s.status,'attempts',s.attempt_count)), '[]'::json) "
                "FROM memory_erasure_delivery_scopes s JOIN memory_erasure_deliveries d ON d.id=s.erasure_delivery_id WHERE d.erasure_id=$$" + receipt_id + "$$::uuid)"
                ")::text"
            )
            raise GateError(f"privacy erasure did not verify managed backup destruction: {receipt_state}")
        if self.decode_int(self.main_sql("SELECT count(*) FROM memory_generation_keys WHERE learner_generation=1 AND destroyed_at IS NOT NULL AND wrapped_key IS NULL"), "destroyed key count") < 1:
            raise GateError("old generation wrapped key was not destroyed")

        status, inventory = self.internal("GET", "/internal/edu-agent/backups", token_mode="maintenance")
        if status != 200 or all(item.get("path") != old_path for item in inventory.get("artifacts", [])):
            raise GateError("destroyed-key artifact disappeared before retention prune")
        self.compose("exec", "-T", "server", "test", "-f", "/var/lib/edu-agent/nocturne-backups/" + old_path)
        self.expect_failure(
            self.restore_artifact_command(old_path),
            "destroyed generation key did not fail the real restore command",
        )
        self.check_destroyed_key_rollback_failure()

        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            status, body = self.internal("GET", "/internal/edu-agent/backups", token_mode="maintenance")
            filesystem = subprocess.run(
                self.compose_base + [
                    "exec", "-T", "server", "test", "-e",
                    "/var/lib/edu-agent/nocturne-backups/" + old_path,
                ],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            pruned = self.main_sql(
                "SELECT (pruned_at IS NOT NULL)::int FROM memory_managed_backup_inventory "
                f"WHERE relative_path={self.sql_text(old_path)}"
            ).strip()
            if (
                status == 200
                and all(item.get("path") != old_path for item in body.get("artifacts", []))
                and filesystem.returncode != 0
                and pruned == "1"
            ):
                return
            time.sleep(2)
        raise GateError("ordinary retention prune did not remove the destroyed-key artifact")

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
        if self.scenario in {"rollback", "backup"}:
            self.check_real_rollback_rehearsal()
            if self.scenario == "backup":
                self.check_backup_encryption_key_destruction_and_prune()
            return
        initial = self.create_memory("I prefer concise compose responses")
        try:
            self.wait_memory_applied(initial["record"]["logical_memory_id"], timeout=120)
        except GateError as exc:
            delivery_id = initial["delivery"]["delivery_id"]
            raise GateError(
                f"initial memory delivery did not converge: {self.delivery_runtime_state(delivery_id)}"
            ) from exc
        if self.scenario == "expiry":
            self.check_real_delivery_expiry_reconciliation()
            return
        if self.scenario == "replay":
            self.check_down_queue_auto_recovery()
            self.check_dead_delivery_replay()
            return
        self.check_real_nocturne_crud_and_absence()
        self.check_real_delivery_expiry_reconciliation()
        self.check_database_account_isolation()
        self.check_down_queue_auto_recovery()
        self.check_dead_delivery_replay()
        self.check_real_rollback_rehearsal()
        self.check_backup_encryption_key_destruction_and_prune()
        self.check_sigterm_shutdown()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--compose-file", required=True)
    parser.add_argument("--override-file", required=True)
    parser.add_argument("--env-file", required=True)
    parser.add_argument("--project", required=True)
    parser.add_argument("--scenario", choices=("full", "rollback", "backup", "expiry", "replay"), default="full")
    return parser.parse_args()


if __name__ == "__main__":
    Gate(parse_args()).execute()
