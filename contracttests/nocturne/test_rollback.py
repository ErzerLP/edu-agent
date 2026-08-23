from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
ROLLBACK_PATH = ROOT / "deploy/nocturne/scripts/rollback.py"
PROBE_PATH = ROOT / "deploy/nocturne/scripts/rollback_probe.py"
SHELL_PATH = ROOT / "deploy/nocturne/scripts/rollback.sh"
RUNBOOK_PATH = ROOT / "deploy/nocturne/ROLLBACK.md"


def load_module(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"could not load {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


rollback = load_module("edu_agent_rollback", ROLLBACK_PATH)
probe = load_module("edu_agent_rollback_probe", PROBE_PATH)


class RollbackValidationTests(unittest.TestCase):
    def test_rejects_floating_image_original_volume_and_nonempty_target(self):
        for value in ("registry.example/nocturne:2.5.6", "registry.example/nocturne@sha256:ABC", "sha256:" + "a" * 64):
            with self.assertRaises(rollback.RollbackError):
                rollback.validate_image_reference(value)
        canonical = "registry.example/nocturne@sha256:" + "a" * 64
        self.assertEqual(rollback.validate_image_reference(canonical)[1], "sha256:" + "a" * 64)
        with self.assertRaises(rollback.RollbackError):
            rollback.validate_target_volume("project_nocturne-postgres-data", "project_nocturne-postgres-data")
        with self.assertRaises(rollback.RollbackError):
            rollback.assert_target_database_empty("1")
        rollback.assert_target_database_empty("0\n")

    def test_script_contains_supported_restore_isolation_and_digest_checks(self):
        source = ROLLBACK_PATH.read_text(encoding="utf-8")
        shell = SHELL_PATH.read_text(encoding="utf-8")
        required = [
            "repository@sha256", "expected_config_digest", "expected_platform",
            'compose("stop", "server"', 'compose("stop", "nocturne"', "nocturne-backup restore --artifact",
            "/dev/shm/edu-agent-rollback.dump", "pg_restore --exit-on-error", "assert_target_database_empty",
            "source == self.original_volume", 'compose("stop", "nocturne-postgres"',
            'compose("up", "-d", "--no-deps", "nocturne"', "expected_schema_version",
            "rollback_probe.py", "failed-isolated", "bridge writer remains stopped",
        ]
        for marker in required:
            self.assertIn(marker, source)
        self.assertNotIn("docker volume rm", source)
        self.assertNotIn("down --volumes", source)
        self.assertIn("set -eu", shell)
        runbook = RUNBOOK_PATH.read_text(encoding="utf-8")
        self.assertIn("no supported down migration", runbook)
        self.assertIn("container-only downgrade", runbook)
        self.assertIn("failed-isolated", runbook)
        self.assertIn("A84 two-version rehearsal", runbook)
        self.assertIn("failed-forward release", runbook)
        self.assertNotIn("same locked digest used as the old image", runbook)
        self.assertIn("distinct locked old and failed-forward images", runbook)
        self.assertIn("transactionally renames the live `nodes` table", runbook)


class ProbeHandler(BaseHTTPRequestHandler):
    seed_uuid = "11111111-1111-4111-8111-111111111111"
    seed_path = "edu-agent/seed"
    seed_content = "pre-upgrade seed content"
    nodes = {seed_path: {"node_uuid": seed_uuid, "content": seed_content}}

    def log_message(self, format: str, *args):
        del format, args

    def body(self) -> dict:
        length = int(self.headers.get("Content-Length", "0"))
        value = json.loads(self.rfile.read(length)) if length else {}
        if not isinstance(value, dict):
            raise ValueError("request body must be an object")
        return value

    def send_json(self, status: int, value):
        payload = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def authorized(self, maintenance: bool = False) -> bool:
        expected = "Bearer maintenance-token" if maintenance else "Bearer api-token"
        return self.headers.get("Authorization") == expected

    def do_GET(self):
        parsed = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(parsed.query)
        if parsed.path == "/health":
            self.send_json(200, {"status": "ok", "database": "connected"})
            return
        if parsed.path == "/internal/edu-agent/capabilities":
            if not self.authorized(maintenance=True):
                self.send_json(401, {"detail": "Unauthorized"})
                return
            self.send_json(200, {"upstream_commit": "a" * 40, "compat_revision": "old-compat", "boot_epoch": "22222222-2222-4222-8222-222222222222"})
            return
        if parsed.path.startswith("/internal/edu-agent/nodes/") and parsed.path.endswith("/references"):
            if not self.authorized(maintenance=True):
                self.send_json(401, {"detail": "Unauthorized"})
                return
            node_uuid = parsed.path.split("/")[4]
            self.send_json(200, {"node_uuid": node_uuid, "complete": True})
            return
        if not self.authorized():
            self.send_json(401, {"detail": "Unauthorized"})
            return
        if parsed.path == "/api/browse/node":
            path = query.get("path", [""])[0]
            node = self.nodes.get(path)
            if node is None:
                self.send_json(404, {"detail": "Not found"})
                return
            self.send_json(200, {"node": {"path": path, **node}})
            return
        if parsed.path == "/api/browse/search":
            search = query.get("q", [""])[0]
            results = [
                {"path": path, "domain": "core", "uri": "core://" + path, "name": path.rsplit("/", 1)[-1], "snippet": node["content"], "priority": 0}
                for path, node in self.nodes.items() if search in node["content"] or search in path
            ]
            self.send_json(200, {"query": search, "count": len(results), "results": results})
            return
        self.send_json(404, {"detail": "Not found"})

    def do_POST(self):
        if self.path != "/api/browse/node" or not self.authorized():
            self.send_json(401, {"detail": "Unauthorized"})
            return
        body = self.body()
        path = "edu-agent/" + body["title"]
        self.nodes[path] = {"node_uuid": body["title"], "content": body["content"]}
        self.send_json(200, {"success": True, "uri": "core://" + path, "memory_id": 10})

    def do_PUT(self):
        parsed = urllib.parse.urlsplit(self.path)
        if parsed.path != "/api/browse/node" or not self.authorized():
            self.send_json(401, {"detail": "Unauthorized"})
            return
        path = urllib.parse.parse_qs(parsed.query).get("path", [""])[0]
        self.nodes[path]["content"] = self.body()["content"]
        self.send_json(200, {"success": True, "memory_id": 11})

    def do_DELETE(self):
        parsed = urllib.parse.urlsplit(self.path)
        if parsed.path != "/api/browse/node" or not self.authorized():
            self.send_json(401, {"detail": "Unauthorized"})
            return
        path = urllib.parse.parse_qs(parsed.query).get("path", [""])[0]
        self.nodes.pop(path, None)
        self.send_json(200, {"success": True, "uri": "core://" + path})


class RollbackProbeTests(unittest.TestCase):
    def setUp(self):
        ProbeHandler.nodes = {
            ProbeHandler.seed_path: {"node_uuid": ProbeHandler.seed_uuid, "content": ProbeHandler.seed_content}
        }
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), ProbeHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.original_base = getattr(probe, "BASE_URL", None)
        setattr(probe, "BASE_URL", f"http://127.0.0.1:{self.server.server_port}")
        self.original_api = os.environ.get("API_TOKEN")
        self.original_maintenance = os.environ.get("EDU_AGENT_MAINTENANCE_TOKEN")
        os.environ["API_TOKEN"] = "api-token"
        os.environ["EDU_AGENT_MAINTENANCE_TOKEN"] = "maintenance-token"

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        if self.original_base is None:
            delattr(probe, "BASE_URL")
        else:
            setattr(probe, "BASE_URL", self.original_base)
        if self.original_api is None:
            os.environ.pop("API_TOKEN", None)
        else:
            os.environ["API_TOKEN"] = self.original_api
        if self.original_maintenance is None:
            os.environ.pop("EDU_AGENT_MAINTENANCE_TOKEN", None)
        else:
            os.environ["EDU_AGENT_MAINTENANCE_TOKEN"] = self.original_maintenance

    def test_health_capability_seed_and_crud_search_references(self):
        result = probe.verify({
            "expected_upstream_commit": "a" * 40,
            "expected_compat_revision": "old-compat",
            "seed_path": ProbeHandler.seed_path,
            "seed_search_query": "pre-upgrade seed",
            "seed_content_sha256": hashlib.sha256(ProbeHandler.seed_content.encode()).hexdigest(),
        })
        self.assertEqual(result["seed_node_uuid"], ProbeHandler.seed_uuid)
        self.assertEqual(result["validation"], "crud-search-references-passed")
        self.assertEqual(set(ProbeHandler.nodes), {ProbeHandler.seed_path})


if __name__ == "__main__":
    unittest.main()
