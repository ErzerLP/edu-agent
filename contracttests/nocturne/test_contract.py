# pyright: reportMissingImports=false

from __future__ import annotations

import asyncio
import hashlib
import importlib
import json
import os
import sys
from pathlib import Path
from uuid import UUID

import pytest
from httpx import ASGITransport, AsyncClient

SOURCE = Path(os.environ["NOCTURNE_SOURCE_DIR"])
sys.path.insert(0, str(SOURCE))

from edu_agent_maintenance import validate_startup_secrets

API_TOKEN = "api-token-0123456789-0123456789-abcdef"
MAINTENANCE_TOKEN = "maintenance-token-0123456789-0123456789-abcdef"
NODE = "11111111-1111-4111-8111-111111111111"
CHILD = "22222222-2222-4222-8222-222222222222"
ROOT_NODE = "00000000-0000-0000-0000-000000000000"
PRUNE_OPERATION = "55555555-5555-4555-8555-555555555555"
INVENTORY_FORMAT = "edu-agent-managed-backup-inventory-v1"


def manifest_digest(artifacts: list[dict]) -> str:
    ordered = sorted(artifacts, key=lambda item: (item["created_at"], item["path"]))
    payload = json.dumps({"format": INVENTORY_FORMAT, "artifacts": ordered}, sort_keys=True, separators=(",", ":")).encode() + b"\n"
    return hashlib.sha256(payload).hexdigest()


def prune_body(digest: str, paths: list[str], *, operation_id: str = PRUNE_OPERATION) -> dict:
    return {
        "operation_id": operation_id,
        "cutoff": "2026-01-02T00:00:00Z",
        "expected_manifest_sha256": digest,
        "paths": paths,
    }


def test_startup_rejects_missing_weak_and_reused_tokens():
    with pytest.raises(RuntimeError):
        validate_startup_secrets("", MAINTENANCE_TOKEN)
    with pytest.raises(RuntimeError):
        validate_startup_secrets(API_TOKEN, "short")
    with pytest.raises(RuntimeError):
        validate_startup_secrets(API_TOKEN, API_TOKEN)
    assert validate_startup_secrets(API_TOKEN, MAINTENANCE_TOKEN) == (API_TOKEN, MAINTENANCE_TOKEN)


def test_production_boundary_forwards_lifespan_scope():
    from web_app import ProductionBoundary

    observed = []

    async def inner(scope, receive, send):
        observed.append(scope["type"])

    async def invoke():
        boundary = ProductionBoundary(inner, API_TOKEN, MAINTENANCE_TOKEN)
        await boundary({"type": "lifespan"}, lambda: None, lambda message: None)

    asyncio.run(invoke())
    assert observed == ["lifespan"]


def test_default_lifespan_installs_managed_migration_runner(monkeypatch):
    import web_app

    observed = []

    class DatabaseManager:
        async def init_db(self):
            import db.migrations.runner as migration_runner
            observed.append(migration_runner.run_migrations is web_app._managed_run_migrations)

    class PresetService:
        async def auto_promote_from_config(self):
            return None

    async def close_database():
        return None

    monkeypatch.setattr(web_app._cfg, "ensure_config_exists", lambda: None)
    monkeypatch.setattr(web_app, "get_db_manager", lambda: DatabaseManager())
    monkeypatch.setattr(web_app, "get_preset_service", lambda: PresetService())
    monkeypatch.setattr(web_app, "close_db", close_database)

    async def exercise():
        async with web_app.default_lifespan(None):
            pass

    asyncio.run(exercise())
    assert observed == [True]


def test_upgrade_backup_must_be_recent_and_bound_to_the_upgrade_window(monkeypatch):
    from datetime import datetime, timedelta, timezone
    import web_app

    checked_at = datetime(2026, 9, 1, 12, 0, 0, tzinfo=timezone.utc)
    not_before = checked_at - timedelta(minutes=10)
    artifact = {"created_at": "2026-09-01T11:49:59Z"}
    monkeypatch.setattr(web_app, "validated_inventory", lambda: (Path("/backups"), [artifact], "0" * 64))
    with pytest.raises(RuntimeError, match="not fresh"):
        web_app._require_encrypted_managed_backup(not_before=not_before, checked_at=checked_at)

    artifact["created_at"] = "2026-09-01T11:50:00Z"
    assert web_app._require_encrypted_managed_backup(not_before=not_before, checked_at=checked_at) == "0" * 64
    artifact["created_at"] = "2026-09-01T11:50:00+00:00"
    with pytest.raises(RuntimeError, match="valid UTC artifact"):
        web_app._require_encrypted_managed_backup(not_before=not_before, checked_at=checked_at)


def test_existing_database_requires_managed_encrypted_migration_backup(tmp_path, monkeypatch):
    from sqlalchemy import Integer
    from sqlalchemy.ext.asyncio import AsyncSession, create_async_engine
    from sqlalchemy.orm import DeclarativeBase, mapped_column
    import web_app

    class LegacyBase(DeclarativeBase):
        pass

    class LegacyContent(LegacyBase):
        __tablename__ = "legacy_content"
        id = mapped_column(Integer, primary_key=True)

    async def exercise():
        engine = create_async_engine(f"sqlite+aiosqlite:///{tmp_path / 'existing.db'}")
        try:
            async with engine.begin() as connection:
                await connection.run_sync(LegacyBase.metadata.create_all)
            async with AsyncSession(engine) as session:
                async with session.begin():
                    session.add(LegacyContent(id=1))
            monkeypatch.setattr(web_app, "validated_inventory", lambda: (tmp_path, [], "0" * 64))
            with pytest.raises(RuntimeError, match="managed encrypted migration backup is required"):
                await web_app._managed_run_migrations(engine)
        finally:
            await engine.dispose()

    asyncio.run(exercise())
    assert not list(tmp_path.glob("*.sql"))
    assert not list(tmp_path.glob("*.json"))


def test_migration_lease_http_contract(monkeypatch):
    import web_app

    operation_id = "11111111-1111-4111-8111-111111111111"
    backup_identity = "a" * 64
    monkeypatch.setenv("EDU_AGENT_SERVER_INTERNAL_URL", "http://server:8080")
    monkeypatch.setenv("EDU_AGENT_SERVER_MAINTENANCE_TOKEN", MAINTENANCE_TOKEN)
    monkeypatch.setenv("EDU_AGENT_MAINTENANCE_TOKEN", MAINTENANCE_TOKEN)
    observed = []

    class Response:
        def __init__(self, status, body=b""):
            self.status = status
            self.body = body

        def __enter__(self):
            return self

        def __exit__(self, *_):
            return False

        def read(self):
            return self.body

    def urlopen(request, timeout):
        observed.append((request.full_url, request.get_header("Authorization"), request.data, timeout))
        if request.full_url.endswith("/acquire"):
            return Response(200, json.dumps({
                "operation_id": operation_id, "status": "acquired", "replayed": False,
            }).encode())
        return Response(204)

    monkeypatch.setattr(web_app.urllib.request, "urlopen", urlopen)
    web_app._migration_lease_request("acquire", operation_id, backup_identity)
    web_app._migration_lease_request("release", operation_id, backup_identity)
    assert [item[0].rsplit("/", 1)[-1] for item in observed] == ["acquire", "release"]
    assert all(item[1] == f"Bearer {MAINTENANCE_TOKEN}" and item[3] == 10 for item in observed)
    assert all(json.loads(item[2]) == {"operation_id": operation_id, "backup_identity": backup_identity} for item in observed)

    monkeypatch.setenv("EDU_AGENT_SERVER_MAINTENANCE_TOKEN", API_TOKEN)
    with pytest.raises(RuntimeError, match="configuration is invalid"):
        web_app._migration_lease_request("acquire", operation_id, backup_identity)


def test_existing_migration_lease_recovers_after_release_crash(tmp_path, monkeypatch):
    from sqlalchemy import Integer
    from sqlalchemy.ext.asyncio import AsyncSession, create_async_engine
    from sqlalchemy.orm import DeclarativeBase, mapped_column
    import web_app
    from db.models import Base as NocturneBase

    class LegacyBase(DeclarativeBase):
        pass

    class LegacyContent(LegacyBase):
        __tablename__ = "legacy_content_for_lease"
        id = mapped_column(Integer, primary_key=True)

    async def exercise():
        engine = create_async_engine(f"sqlite+aiosqlite:///{tmp_path / 'lease-existing.db'}")
        try:
            async with engine.begin() as connection:
                await connection.run_sync(NocturneBase.metadata.create_all)
                await connection.run_sync(LegacyBase.metadata.create_all)
            async with AsyncSession(engine) as session:
                async with session.begin():
                    session.add(LegacyContent(id=1))

            backup_identity = "b" * 64
            monkeypatch.setattr(web_app, "_require_encrypted_managed_backup", lambda **_: backup_identity)
            calls = []
            fail_release = True

            def lease_request(action, operation_id, identity):
                nonlocal fail_release
                calls.append((action, operation_id, identity))
                if action == "release" and fail_release:
                    fail_release = False
                    raise RuntimeError("injected release outage")

            monkeypatch.setattr(web_app, "_migration_lease_request", lease_request)
            with pytest.raises(RuntimeError, match="injected release outage"):
                await web_app._managed_run_migrations(engine)
            async with AsyncSession(engine) as session:
                state = await session.get(web_app._MigrationLeaseState, 1)
                assert state is not None
                stored = (state.operation_id, state.backup_identity)
            assert calls[0] == ("acquire", stored[0], backup_identity)
            assert calls[1] == ("release", stored[0], backup_identity)

            await web_app._managed_run_migrations(engine)
            assert calls[2] == ("release", stored[0], backup_identity)
            async with AsyncSession(engine) as session:
                assert await session.get(web_app._MigrationLeaseState, 1) is None
        finally:
            await engine.dispose()

    asyncio.run(exercise())


def test_fresh_database_migrations_do_not_acquire_lease(tmp_path, monkeypatch):
    from sqlalchemy.ext.asyncio import create_async_engine
    import web_app
    from db.models import Base as NocturneBase

    async def exercise():
        engine = create_async_engine(f"sqlite+aiosqlite:///{tmp_path / 'fresh.db'}")
        try:
            async with engine.begin() as connection:
                await connection.run_sync(NocturneBase.metadata.create_all)
            monkeypatch.setattr(web_app, "_migration_lease_request", lambda *_: pytest.fail("fresh database acquired migration lease"))
            await web_app._managed_run_migrations(engine)
        finally:
            await engine.dispose()

    asyncio.run(exercise())


def test_fixed_overlay_contract(tmp_path, monkeypatch):
    asyncio.run(_run_contract(tmp_path, monkeypatch))


async def _run_contract(tmp_path: Path, monkeypatch):
    database = tmp_path / "nocturne.db"
    snapshots = tmp_path / "snapshots"
    backups = tmp_path / "backups"
    snapshots.mkdir()
    backups.mkdir()
    config_path = tmp_path / "config.json"
    config_path.write_text(json.dumps({
        "database_url": f"sqlite+aiosqlite:///{database}",
        "valid_domains": ["core", "notes"],
        "boot_uris": {},
        "host": "0.0.0.0",
        "web_port": 8233,
        "auto_open_browser": False,
        "api_token": API_TOKEN,
        "locale": "en"
    }), encoding="utf-8")
    monkeypatch.setenv("SNAPSHOT_DIR", str(snapshots))
    monkeypatch.setenv("EDU_AGENT_BACKUP_ROOT", str(backups))
    monkeypatch.setenv("EDU_AGENT_MAINTENANCE_TOKEN", MAINTENANCE_TOKEN)

    import config
    config.CONFIG_PATH = config_path
    config._invalidate()
    import db
    import db.snapshot as snapshot_module
    import web_app
    monkeypatch.setattr(web_app, "validated_inventory", lambda: (backups, [{"path": "fixture.backup.enc"}], "0" * 64))
    await db.close_db()
    snapshot_module._store = None
    manager = db.get_db_manager()
    await manager.init_db()
    await db.get_preset_service().auto_promote_from_config()

    from db.models import Edge, GlossaryKeyword, Memory, MemoryAccessLog, Node, Path as PathModel, Preset, SearchDocument
    from sqlalchemy import select
    async with manager.session() as session:
        session.add_all([
            Node(uuid=NODE), Node(uuid=CHILD),
            Memory(id=1, node_uuid=NODE, content="old", deprecated=True, migrated_to=2),
            Memory(id=2, node_uuid=NODE, content="stable preference", deprecated=False),
            Memory(id=3, node_uuid=CHILD, content="child", deprecated=False),
            Edge(id=10, parent_uuid=ROOT_NODE, child_uuid=NODE, name="memory", priority=1, disclosure="test"),
            Edge(id=11, parent_uuid=NODE, child_uuid=CHILD, name="child", priority=1, disclosure="test"),
            PathModel(namespace="ns-a", domain="core", path="edu-agent/memory", edge_id=10, node_uuid=NODE),
            PathModel(namespace="ns-b", domain="notes", path="alias-memory", edge_id=10, node_uuid=NODE),
            PathModel(namespace="ns-conflict", domain="core", path="edu-agent/memory", edge_id=11, node_uuid=CHILD),
            GlossaryKeyword(id=20, keyword="preference", node_uuid=NODE, namespace="ns-b"),
        ])
        await session.flush()
        session.add_all([
            SearchDocument(namespace="ns-a", domain="core", path="edu-agent/memory", node_uuid=NODE, memory_id=2, uri="core://edu-agent/memory", content="stable preference", disclosure="test", search_terms="preference", priority=1),
            SearchDocument(namespace="ns-b", domain="notes", path="alias-memory", node_uuid=NODE, memory_id=2, uri="notes://alias-memory", content="stable preference", disclosure="test", search_terms="preference", priority=1),
            MemoryAccessLog(id=100, node_uuid=NODE, namespace="ns-a", context="test"),
        ])
        preset = (await session.execute(select(Preset).where(Preset.is_active == True))).scalar_one()  # noqa: E712
        preset.boot_uris = json.dumps({
            "ns-a": ["core://edu-agent/memory"],
            "ns-b": ["notes://alias-memory"],
            "ns-conflict": ["core://edu-agent/memory"],
        })

    changes = db.get_changeset_store()
    changes.record("nodes", None, {"uuid": NODE})
    changes.record("memories", None, {"id": 1, "node_uuid": NODE})
    changes.record("edges", None, {"id": 10, "parent_uuid": ROOT_NODE, "child_uuid": NODE})
    changes.record("paths", None, {"namespace": "ns-a", "domain": "core", "path": "edu-agent/memory", "edge_id": 10, "node_uuid": NODE})
    changes.record("nodes", None, {"uuid": CHILD})

    web_app = importlib.reload(importlib.import_module("web_app"))
    app = web_app.build_web_app()
    api_headers = {"Authorization": f"Bearer {API_TOKEN}", "X-Namespace": "ns-a"}
    maintenance_headers = {"Authorization": f"Bearer {MAINTENANCE_TOKEN}", "X-Namespace": "ignored-by-internal-extension"}
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://testserver") as client:
        assert (await client.get("/health")).json() == {"status": "ok", "database": "connected"}
        assert (await client.get("/api/browse/node", params={"domain": "core", "path": "edu-agent/memory"})).status_code == 401
        assert (await client.get("/api/browse/node", params={"domain": "core", "path": "edu-agent/memory"}, headers=maintenance_headers)).status_code == 401
        browse = await client.get("/api/browse/node", params={"domain": "core", "path": "edu-agent/memory"}, headers=api_headers)
        assert browse.status_code == 200 and browse.json()["node"]["node_uuid"] == NODE
        assert (await client.get("/internal/edu-agent/capabilities", headers=api_headers)).status_code == 401
        assert (await client.get("/internal/edu-agent/capabilities")).status_code == 401
        capabilities = (await client.get("/internal/edu-agent/capabilities", headers=maintenance_headers)).json()
        assert set(capabilities) == {"upstream_commit", "compat_revision", "boot_epoch"}
        assert capabilities["upstream_commit"] == "54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254"
        assert capabilities["compat_revision"] == "edu-agent-maintenance-v1"
        assert str(UUID(capabilities["boot_epoch"])) == capabilities["boot_epoch"]
        assert (await client.get("/internal/edu-agent/capabilities", headers=maintenance_headers)).json()["boot_epoch"] == capabilities["boot_epoch"]

        refs_response = await client.get(f"/internal/edu-agent/nodes/{NODE}/references", headers=maintenance_headers)
        assert refs_response.status_code == 200
        assert refs_response.json() == {
            "node_uuid": NODE, "complete": True, "active_memory_id": 2, "memory_ids": [1, 2],
            "paths": [
                {"namespace": "ns-a", "domain": "core", "path": "edu-agent/memory", "uri": "core://edu-agent/memory", "alias": False},
                {"namespace": "ns-b", "domain": "notes", "path": "alias-memory", "uri": "notes://alias-memory", "alias": True},
            ],
            "edge_ids": ["10", "11"], "glossary_keywords": ["preference"],
            "search_document_ids": ["ns-a|core://edu-agent/memory", "ns-b|notes://alias-memory"],
            "access_log_ids": ["100"],
            "boot_uris": [
                {"preset": "default", "namespace": "ns-a", "uri": "core://edu-agent/memory"},
                {"preset": "default", "namespace": "ns-b", "uri": "notes://alias-memory"},
            ],
            "review_references": [f"edges:10", "memories:1", f"nodes:{NODE}", "paths:ns-a|core|edu-agent/memory"],
        }
        assert (await client.get(f"/internal/edu-agent/nodes/{NODE}/references?namespace=ns-a", headers=maintenance_headers)).status_code == 422
        cleanup = await client.delete(f"/internal/edu-agent/nodes/{NODE}/review-reference", headers=maintenance_headers)
        assert cleanup.status_code == 200 and cleanup.json() == {"success": True}
        remaining, _ = changes.get_snapshot_view()
        assert set(remaining) == {f"nodes:{CHILD}"}

        for path, method in [
            ("/api/settings", "GET"), ("/api/browse/domains", "GET"), ("/api/review/groups", "GET"),
            ("/docs", "GET"), ("/sse", "GET"), ("/messages", "POST"), ("/mcp", "POST"),
            ("/api/browse/node/alias", "POST"), ("/api/maintenance/access-logs", "DELETE"),
            ("/api/maintenance/orphans/0", "GET"),
        ]:
            assert (await client.request(method, path, headers=api_headers)).status_code == 404
        extra = await client.post("/api/browse/node", headers=api_headers, json={"parent_path": "", "content": "x", "priority": 1, "disclosure": "x", "title": "x", "domain": "core", "unexpected": True})
        assert extra.status_code == 422
        empty_digest = manifest_digest([])
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).json() == {
            "validated": True, "manifest_sha256": empty_digest, "artifacts": []
        }

        # A missing manifest is valid only when the root has no non-control entries.
        rogue = backups / "unregistered.enc"
        rogue.write_bytes(b"must survive")
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).status_code == 409
        refused = await client.post("/internal/edu-agent/backups/prune", headers=maintenance_headers, json=prune_body("0" * 64, []))
        assert refused.status_code == 409 and rogue.read_bytes() == b"must survive"
        rogue.unlink()

        outside = tmp_path / "outside.enc"
        outside.write_bytes(b"outside")
        manifest = backups / "managed-inventory.json"
        bad = {"format": "edu-agent-managed-backup-inventory-v1", "artifacts": [{"path": "../outside.enc", "created_at": "2026-01-01T00:00:00Z", "size": 7, "sha256": hashlib.sha256(b"outside").hexdigest(), "learner_generation": 1, "wrapped_key_id": "33333333-3333-4333-8333-333333333333"}]}
        manifest.write_text(json.dumps(bad), encoding="utf-8")
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).status_code == 409
        manifest.unlink()

        (backups / "escape.enc").symlink_to(outside)
        bad["artifacts"][0]["path"] = "escape.enc"
        manifest.write_text(json.dumps(bad), encoding="utf-8")
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).status_code == 409
        (backups / "escape.enc").unlink()
        manifest.unlink()

        manifest.symlink_to(outside)
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).status_code == 409
        manifest.unlink()

        real_parent = tmp_path / "real-parent"
        real_backups = real_parent / "backups"
        real_backups.mkdir(parents=True)
        linked_parent = tmp_path / "linked-parent"
        linked_parent.symlink_to(real_parent, target_is_directory=True)
        monkeypatch.setenv("EDU_AGENT_BACKUP_ROOT", str(linked_parent / "backups"))
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).status_code == 409
        monkeypatch.setenv("EDU_AGENT_BACKUP_ROOT", str(backups))

        old, new = backups / "old.enc", backups / "new.enc"
        old.write_bytes(b"old encrypted artifact")
        new.write_bytes(b"new encrypted artifact")
        artifacts = [
            {"path": "old.enc", "created_at": "2026-01-01T00:00:00Z", "size": old.stat().st_size, "sha256": hashlib.sha256(old.read_bytes()).hexdigest(), "learner_generation": 1, "wrapped_key_id": "33333333-3333-4333-8333-333333333333"},
            {"path": "new.enc", "created_at": "2026-01-03T00:00:00Z", "size": new.stat().st_size, "sha256": hashlib.sha256(new.read_bytes()).hexdigest(), "learner_generation": 2, "wrapped_key_id": "44444444-4444-4444-8444-444444444444"},
        ]
        manifest_payload = {"format": "edu-agent-managed-backup-inventory-v1", "artifacts": artifacts}
        manifest.write_text(json.dumps(manifest_payload), encoding="utf-8")
        inventory = await client.get("/internal/edu-agent/backups", headers=maintenance_headers)
        current_digest = manifest_digest(artifacts)
        assert inventory.status_code == 200 and inventory.json() == {
            "validated": True, "manifest_sha256": current_digest, "artifacts": artifacts
        }

        # Every manifest-external file, symlink, and directory invalidates the inventory.
        extra_regular = backups / "extra.enc"
        extra_regular.write_bytes(b"extra")
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).status_code == 409
        refused = await client.post("/internal/edu-agent/backups/prune", headers=maintenance_headers, json=prune_body(current_digest, ["old.enc"]))
        assert refused.status_code == 409 and old.exists() and extra_regular.exists()
        extra_regular.unlink()
        extra_symlink = backups / "extra-link"
        extra_symlink.symlink_to(outside)
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).status_code == 409
        extra_symlink.unlink()
        extra_directory = backups / "extra-directory"
        extra_directory.mkdir()
        assert (await client.get("/internal/edu-agent/backups", headers=maintenance_headers)).status_code == 409
        extra_directory.rmdir()

        import edu_agent_maintenance as maintenance

        # Replacing an immutable name with identical bytes still changes its inode and aborts prune.
        original_revalidate = maintenance._revalidate_artifact
        revalidate_calls = 0

        def replace_before_unlink(root, item, expected):
            nonlocal revalidate_calls
            revalidate_calls += 1
            if revalidate_calls == 1:
                replacement = backups / "producer-replacement.tmp"
                replacement.write_bytes(old.read_bytes())
                os.replace(replacement, old)
            return original_revalidate(root, item, expected)

        monkeypatch.setattr(maintenance, "_revalidate_artifact", replace_before_unlink)
        raced = await client.post("/internal/edu-agent/backups/prune", headers=maintenance_headers, json=prune_body(current_digest, ["old.enc"]))
        assert raced.status_code == 409 and old.exists() and json.loads(manifest.read_text(encoding="utf-8")) == manifest_payload
        monkeypatch.setattr(maintenance, "_revalidate_artifact", original_revalidate)

        # A producer that changes the manifest outside the shared flock cannot be overwritten.
        original_manifest_check = maintenance._assert_manifest_unchanged
        manifest_checks = 0
        producer_payload = json.dumps({"format": "producer-won", "artifacts": artifacts}).encode()

        def replace_manifest_before_cas(root, expected):
            nonlocal manifest_checks
            manifest_checks += 1
            if manifest_checks == 2:
                replacement = backups / "producer-manifest.tmp"
                replacement.write_bytes(producer_payload)
                os.replace(replacement, manifest)
            return original_manifest_check(root, expected)

        monkeypatch.setattr(maintenance, "_assert_manifest_unchanged", replace_manifest_before_cas)
        raced = await client.post("/internal/edu-agent/backups/prune", headers=maintenance_headers, json=prune_body(current_digest, ["old.enc"]))
        assert raced.status_code == 409 and old.exists() and manifest.read_bytes() == producer_payload
        monkeypatch.setattr(maintenance, "_assert_manifest_unchanged", original_manifest_check)
        manifest.write_text(json.dumps(manifest_payload), encoding="utf-8")

        # Replacing the named flock inode must fail before any artifact is unlinked.
        original_lock_check = maintenance._assert_lock_unchanged
        lock_checks = 0

        def replace_lock_before_unlink(root):
            nonlocal lock_checks
            lock_checks += 1
            if lock_checks == 3:
                replacement = backups / "producer-lock.tmp"
                replacement.write_bytes(b"replacement lock")
                os.replace(replacement, backups / maintenance.INVENTORY_LOCK_NAME)
            return original_lock_check(root)

        monkeypatch.setattr(maintenance, "_assert_lock_unchanged", replace_lock_before_unlink)
        raced = await client.post("/internal/edu-agent/backups/prune", headers=maintenance_headers, json=prune_body(current_digest, ["old.enc"]))
        assert raced.status_code == 409 and old.exists() and json.loads(manifest.read_text(encoding="utf-8")) == manifest_payload
        monkeypatch.setattr(maintenance, "_assert_lock_unchanged", original_lock_check)

        prune = await client.post("/internal/edu-agent/backups/prune", headers=maintenance_headers, json=prune_body(current_digest, ["old.enc"]))
        new_digest = manifest_digest([artifacts[1]])
        assert prune.status_code == 200 and prune.json() == {
            "operation_id": PRUNE_OPERATION, "deleted_paths": ["old.enc"], "manifest_sha256": new_digest
        }
        assert not old.exists() and new.exists()
        no_op = prune_body(new_digest, [], operation_id="66666666-6666-4666-8666-666666666666")
        assert (await client.post("/internal/edu-agent/backups/prune", headers=maintenance_headers, json=no_op)).json() == {
            "operation_id": no_op["operation_id"], "deleted_paths": [], "manifest_sha256": new_digest
        }
        assert (await client.get("/internal/edu-agent/backups?namespace=ns-a", headers=maintenance_headers)).status_code == 422
        invalid = dict(no_op, namespace="x")
        assert (await client.post("/internal/edu-agent/backups/prune", headers=maintenance_headers, json=invalid)).status_code == 422

    await db.close_db()
