#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any


BASE_URL = "http://127.0.0.1:8233"


class ProbeError(RuntimeError):
    pass


def request_json(method: str, path: str, token: str = "", body: Any | None = None) -> tuple[int, Any]:
    data = None if body is None else json.dumps(body, separators=(",", ":")).encode("utf-8")
    request = urllib.request.Request(BASE_URL + path, data=data, method=method)
    request.add_header("Accept", "application/json")
    request.add_header("X-Namespace", "edu-agent")
    if data is not None:
        request.add_header("Content-Type", "application/json")
    if token:
        request.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            payload = response.read()
            return response.status, json.loads(payload) if payload else None
    except urllib.error.HTTPError as exc:
        payload = exc.read()
        try:
            decoded = json.loads(payload) if payload else None
        except json.JSONDecodeError:
            decoded = None
        return exc.code, decoded
    except OSError as exc:
        raise ProbeError("Nocturne request failed") from exc


def require_status(result: tuple[int, Any], expected: int, label: str) -> Any:
    status, body = result
    if status != expected:
        raise ProbeError(label)
    return body


def wait_search(api_token: str, query: str, path: str, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    encoded = urllib.parse.urlencode({"q": query, "domain": "core", "limit": "100"})
    while time.monotonic() < deadline:
        status, body = request_json("GET", "/api/browse/search?" + encoded, api_token)
        if status == 200 and isinstance(body, dict):
            results = body.get("results")
            if isinstance(results, list) and any(isinstance(item, dict) and item.get("path") == path for item in results):
                return
        time.sleep(0.5)
    raise ProbeError("Nocturne search did not expose the expected restored record")


def verify(request: dict[str, Any]) -> dict[str, str]:
    required = {
        "expected_upstream_commit", "expected_compat_revision", "seed_path",
        "seed_search_query", "seed_content_sha256",
    }
    if set(request) != required:
        raise ProbeError("rollback probe request is invalid")
    api_token = os.environ.get("API_TOKEN", "")
    maintenance_token = os.environ.get("EDU_AGENT_MAINTENANCE_TOKEN", "")
    if not api_token or not maintenance_token:
        raise ProbeError("rollback probe tokens are unavailable")

    health = require_status(request_json("GET", "/health"), 200, "Nocturne health check failed")
    if health != {"status": "ok", "database": "connected"}:
        raise ProbeError("Nocturne health payload is incompatible")
    capabilities = require_status(
        request_json("GET", "/internal/edu-agent/capabilities", maintenance_token),
        200,
        "Nocturne capabilities check failed",
    )
    if not isinstance(capabilities, dict) or capabilities.get("upstream_commit") != request["expected_upstream_commit"] or capabilities.get("compat_revision") != request["expected_compat_revision"]:
        raise ProbeError("old Nocturne capability identity does not match the rollback record")

    seed_query = urllib.parse.urlencode({"domain": "core", "path": request["seed_path"]})
    seed = require_status(request_json("GET", "/api/browse/node?" + seed_query, api_token), 200, "restored seed record is unavailable")
    try:
        seed_node = seed["node"]
        seed_uuid = seed_node["node_uuid"]
        seed_content = seed_node["content"]
    except (KeyError, TypeError) as exc:
        raise ProbeError("restored seed record is incomplete") from exc
    if str(uuid.UUID(seed_uuid)) != seed_uuid or hashlib.sha256(seed_content.encode("utf-8")).hexdigest() != request["seed_content_sha256"]:
        raise ProbeError("restored seed record does not match the pre-upgrade evidence")
    wait_search(api_token, request["seed_search_query"], request["seed_path"])
    references = require_status(
        request_json("GET", f"/internal/edu-agent/nodes/{seed_uuid}/references", maintenance_token),
        200,
        "restored seed references are unavailable",
    )
    if not isinstance(references, dict) or references.get("node_uuid") != seed_uuid or not references.get("complete"):
        raise ProbeError("restored seed references are incomplete")

    validation_uuid = str(uuid.uuid4())
    validation_path = "edu-agent/" + validation_uuid
    created = False
    try:
        create = {
            "parent_path": "edu-agent", "content": "rollback validation create", "priority": 0,
            "disclosure": "rollback validation", "title": validation_uuid, "domain": "core",
        }
        created_body = require_status(request_json("POST", "/api/browse/node", api_token, create), 200, "rollback validation create failed")
        if not isinstance(created_body, dict) or not isinstance(created_body.get("memory_id"), int):
            raise ProbeError("rollback validation create response is incomplete")
        created = True
        query = urllib.parse.urlencode({"domain": "core", "path": validation_path})
        read = require_status(request_json("GET", "/api/browse/node?" + query, api_token), 200, "rollback validation read failed")
        node_uuid = read.get("node", {}).get("node_uuid") if isinstance(read, dict) else None
        if not isinstance(node_uuid, str):
            raise ProbeError("rollback validation read response is incomplete")
        update = {"content": "rollback validation updated searchable", "priority": 0, "disclosure": "rollback validation"}
        require_status(request_json("PUT", "/api/browse/node?" + query, api_token, update), 200, "rollback validation update failed")
        wait_search(api_token, "updated searchable", validation_path)
        validation_refs = require_status(
            request_json("GET", f"/internal/edu-agent/nodes/{node_uuid}/references", maintenance_token),
            200,
            "rollback validation references failed",
        )
        if not isinstance(validation_refs, dict) or not validation_refs.get("complete"):
            raise ProbeError("rollback validation references are incomplete")
        require_status(request_json("DELETE", "/api/browse/node?" + query, api_token), 200, "rollback validation delete failed")
        created = False
    finally:
        if created:
            query = urllib.parse.urlencode({"domain": "core", "path": validation_path})
            try:
                request_json("DELETE", "/api/browse/node?" + query, api_token)
            except ProbeError:
                pass
    return {"seed_node_uuid": seed_uuid, "validation": "crud-search-references-passed"}


def main() -> int:
    try:
        request = json.load(sys.stdin)
        verify(request)
    except (ProbeError, ValueError, TypeError, json.JSONDecodeError):
        return 1
    print(json.dumps({"status": "validated"}, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
