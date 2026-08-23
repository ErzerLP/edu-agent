# pyright: reportMissingImports=false

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import sys
from pathlib import Path

import pytest
from fastapi import FastAPI
from httpx import ASGITransport, AsyncClient
from pydantic import ValidationError

SOURCE = Path(os.environ["NOCTURNE_SOURCE_DIR"])
sys.path.insert(0, str(SOURCE))

import edu_agent_maintenance as maintenance

FORMAT = "edu-agent-managed-backup-inventory-v1"
CUTOFF = "2026-01-02T00:00:00Z"
OPERATION = "77777777-7777-4777-8777-777777777777"
WRAPPED_KEYS = (
    "11111111-1111-4111-8111-111111111111",
    "22222222-2222-4222-8222-222222222222",
    "33333333-3333-4333-8333-333333333333",
    "44444444-4444-4444-8444-444444444444",
)


class InjectedCrash(RuntimeError):
    pass


def artifact(path: str, created_at: str, payload: bytes, generation: int) -> dict:
    return {
        "path": path,
        "created_at": created_at,
        "size": len(payload),
        "sha256": hashlib.sha256(payload).hexdigest(),
        "learner_generation": generation,
        "wrapped_key_id": WRAPPED_KEYS[generation - 1],
    }


def canonical_payload(artifacts: list[dict]) -> bytes:
    ordered = sorted(artifacts, key=lambda item: (item["created_at"], item["path"]))
    return json.dumps({"format": FORMAT, "artifacts": ordered}, sort_keys=True, separators=(",", ":")).encode() + b"\n"


def canonical_digest(artifacts: list[dict]) -> str:
    return hashlib.sha256(canonical_payload(artifacts)).hexdigest()


def install_inventory(root: Path, definitions: list[tuple[str, str, bytes]]) -> list[dict]:
    artifacts: list[dict] = []
    for index, (path, created_at, payload) in enumerate(definitions, start=1):
        (root / path).write_bytes(payload)
        artifacts.append(artifact(path, created_at, payload, index))
    manifest = {"format": FORMAT, "artifacts": list(reversed(artifacts))}
    (root / maintenance.INVENTORY_NAME).write_text(json.dumps(manifest, indent=2), encoding="utf-8")
    return artifacts


def request_for(digest: str, paths: list[str], *, operation_id: str = OPERATION) -> maintenance.PruneRequest:
    return maintenance.PruneRequest.model_validate({
        "operation_id": operation_id,
        "cutoff": CUTOFF,
        "expected_manifest_sha256": digest,
        "paths": paths,
    })


def assert_inventory_integrity(root: Path, artifacts: list[dict]) -> None:
    manifest = json.loads((root / maintenance.INVENTORY_NAME).read_text(encoding="utf-8"))
    assert {item["path"] for item in manifest["artifacts"]} == {item["path"] for item in artifacts}
    for item in artifacts:
        payload = (root / item["path"]).read_bytes()
        assert len(payload) == item["size"]
        assert hashlib.sha256(payload).hexdigest() == item["sha256"]
    names = {item.name for item in root.iterdir()}
    expected = {maintenance.INVENTORY_LOCK_NAME, maintenance.INVENTORY_NAME} | {item["path"] for item in artifacts}
    assert names == expected


@pytest.fixture
def backup_root(tmp_path: Path, monkeypatch) -> Path:
    root = tmp_path / "backups"
    root.mkdir()
    monkeypatch.setenv("EDU_AGENT_BACKUP_ROOT", str(root))
    return root


@pytest.mark.parametrize(
    ("stage", "rolled_forward"),
    [
        ("after_journal_replace", False),
        ("after_journal_fsync", False),
        ("after_quarantine_rename:0", False),
        ("after_quarantine_fsync:1", False),
        ("before_manifest_replace", False),
        ("after_manifest_replace", True),
        ("after_manifest_fsync", True),
        ("after_quarantine_unlink:1", True),
        ("after_quarantine_cleanup_fsync:1", True),
    ],
)
def test_prune_recovers_every_durable_boundary(backup_root: Path, monkeypatch, stage: str, rolled_forward: bool):
    artifacts = install_inventory(backup_root, [
        ("a.enc", "2026-01-01T00:00:00Z", b"encrypted-a"),
        ("b.enc", "2026-01-01T01:00:00Z", b"encrypted-b"),
        ("c.enc", "2026-01-01T02:00:00Z", b"encrypted-c"),
        ("z.enc", "2026-01-03T00:00:00Z", b"encrypted-z"),
    ])
    _, current, digest = maintenance.validated_inventory()
    assert current == artifacts
    original_fault = maintenance._crash_point

    def crash(point: str) -> None:
        if point == stage:
            raise InjectedCrash(point)

    monkeypatch.setattr(maintenance, "_crash_point", crash)
    with pytest.raises(InjectedCrash, match=stage):
        maintenance.prune_inventory(request_for(digest, ["a.enc", "b.enc", "c.enc"]))
    monkeypatch.setattr(maintenance, "_crash_point", original_fault)

    _, recovered, recovered_digest = maintenance.validated_inventory()
    expected = [artifacts[3]] if rolled_forward else artifacts
    assert recovered == expected
    assert recovered_digest == canonical_digest(expected)
    assert_inventory_integrity(backup_root, expected)


def test_lost_response_is_reconciled_by_get(backup_root: Path, monkeypatch):
    artifacts = install_inventory(backup_root, [
        ("old.enc", "2026-01-01T00:00:00Z", b"old encrypted"),
        ("new.enc", "2026-01-03T00:00:00Z", b"new encrypted"),
    ])
    _, _, digest = maintenance.validated_inventory()
    original_fault = maintenance._crash_point

    def lose_response(point: str) -> None:
        if point == "after_journal_cleanup_fsync":
            raise InjectedCrash("response lost")

    monkeypatch.setattr(maintenance, "_crash_point", lose_response)
    with pytest.raises(InjectedCrash, match="response lost"):
        maintenance.prune_inventory(request_for(digest, ["old.enc"]))
    monkeypatch.setattr(maintenance, "_crash_point", original_fault)

    _, current, current_digest = maintenance.validated_inventory()
    assert current == [artifacts[1]]
    assert current_digest == canonical_digest([artifacts[1]])
    assert_inventory_integrity(backup_root, [artifacts[1]])


def test_digest_drift_and_new_eligible_artifact_delete_nothing(backup_root: Path):
    artifacts = install_inventory(backup_root, [
        ("old.enc", "2026-01-01T00:00:00Z", b"old encrypted"),
        ("new.enc", "2026-01-03T00:00:00Z", b"new encrypted"),
    ])
    _, _, stale_digest = maintenance.validated_inventory()

    added_payload = b"producer added eligible"
    (backup_root / "added.enc").write_bytes(added_payload)
    added = artifact("added.enc", "2026-01-01T12:00:00Z", added_payload, 3)
    current_artifacts = artifacts + [added]
    (backup_root / maintenance.INVENTORY_NAME).write_bytes(canonical_payload(current_artifacts))

    with pytest.raises(maintenance.InventoryError, match="digest changed"):
        maintenance.prune_inventory(request_for(stale_digest, ["old.enc"]))
    assert all((backup_root / item["path"]).exists() for item in current_artifacts)

    current_digest = canonical_digest(current_artifacts)
    with pytest.raises(maintenance.InventoryError, match="exactly match"):
        maintenance.prune_inventory(request_for(current_digest, ["old.enc"]))
    assert all((backup_root / item["path"]).exists() for item in current_artifacts)
    _, current, _ = maintenance.validated_inventory()
    assert {item["path"] for item in current} == {"added.enc", "old.enc", "new.enc"}


def test_journal_symlink_and_hardlink_fail_closed(backup_root: Path, tmp_path: Path):
    artifacts = install_inventory(backup_root, [("old.enc", "2026-01-01T00:00:00Z", b"old encrypted")])
    maintenance.validated_inventory()
    outside = tmp_path / "outside"
    outside.write_text("outside", encoding="ascii")
    (backup_root / maintenance.PRUNE_JOURNAL_NAME).symlink_to(outside)
    with pytest.raises(maintenance.InventoryError):
        maintenance.validated_inventory()
    assert (backup_root / "old.enc").exists() and outside.read_text(encoding="ascii") == "outside"
    (backup_root / maintenance.PRUNE_JOURNAL_NAME).unlink()

    hardlink = tmp_path / "old-hardlink.enc"
    os.link(backup_root / "old.enc", hardlink)
    with pytest.raises(maintenance.InventoryError, match="single-link"):
        maintenance.validated_inventory()
    assert hashlib.sha256((backup_root / "old.enc").read_bytes()).hexdigest() == artifacts[0]["sha256"]


@pytest.mark.parametrize("tamper", ["journal", "quarantine_symlink", "quarantine_bytes"])
def test_in_progress_prune_tamper_fails_closed(backup_root: Path, tmp_path: Path, monkeypatch, tamper: str):
    artifacts = install_inventory(backup_root, [
        ("old.enc", "2026-01-01T00:00:00Z", b"old encrypted"),
        ("new.enc", "2026-01-03T00:00:00Z", b"new encrypted"),
    ])
    _, _, digest = maintenance.validated_inventory()
    original_fault = maintenance._crash_point

    def crash(point: str) -> None:
        if point == "after_quarantine_fsync:0":
            raise InjectedCrash(point)

    monkeypatch.setattr(maintenance, "_crash_point", crash)
    with pytest.raises(InjectedCrash):
        maintenance.prune_inventory(request_for(digest, ["old.enc"]))
    monkeypatch.setattr(maintenance, "_crash_point", original_fault)

    journal_path = backup_root / maintenance.PRUNE_JOURNAL_NAME
    journal = json.loads(journal_path.read_text(encoding="utf-8"))
    quarantine_path = backup_root / journal["quarantine"][0]["quarantine"]
    if tamper == "journal":
        journal["operation_id"] = "88888888-8888-4888-8888-888888888888"
        journal_path.write_text(json.dumps(journal), encoding="utf-8")
    elif tamper == "quarantine_symlink":
        outside = tmp_path / "outside-artifact"
        outside.write_bytes(b"outside")
        quarantine_path.unlink()
        quarantine_path.symlink_to(outside)
    else:
        quarantine_path.write_bytes(b"tampered")

    with pytest.raises(maintenance.InventoryError):
        maintenance.validated_inventory()
    assert (backup_root / "new.enc").read_bytes() == b"new encrypted"
    assert json.loads((backup_root / maintenance.INVENTORY_NAME).read_text(encoding="utf-8"))["artifacts"]
    assert artifacts[0]["path"] == "old.enc"


def test_post_schema_and_precise_success_response(backup_root: Path):
    artifacts = install_inventory(backup_root, [
        ("old.enc", "2026-01-01T00:00:00Z", b"old encrypted"),
        ("new.enc", "2026-01-03T00:00:00Z", b"new encrypted"),
    ])
    asyncio.run(_exercise_http_schema(backup_root, artifacts))


async def _exercise_http_schema(backup_root: Path, artifacts: list[dict]) -> None:
    app = FastAPI()
    app.include_router(maintenance.router, prefix="/internal")
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
        inventory = await client.get("/internal/edu-agent/backups")
        assert inventory.status_code == 200
        body = inventory.json()
        assert body == {
            "validated": True,
            "manifest_sha256": canonical_digest(artifacts),
            "artifacts": artifacts,
        }
        valid = {
            "operation_id": OPERATION,
            "cutoff": CUTOFF,
            "expected_manifest_sha256": body["manifest_sha256"],
            "paths": ["old.enc"],
        }
        invalid_bodies = [
            dict(valid, unexpected=True),
            dict(valid, operation_id=None),
            dict(valid, cutoff=None),
            dict(valid, expected_manifest_sha256=None),
            dict(valid, paths=None),
            dict(valid, operation_id="AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"),
            dict(valid, expected_manifest_sha256="A" * 64),
            dict(valid, paths=["old.enc", "old.enc"]),
            dict(valid, paths=["z.enc", "a.enc"]),
            dict(valid, paths=["../old.enc"]),
            dict(valid, paths=[maintenance.PRUNE_JOURNAL_NAME]),
        ]
        for invalid in invalid_bodies:
            response = await client.post("/internal/edu-agent/backups/prune", json=invalid)
            assert response.status_code == 422, invalid

        response = await client.post("/internal/edu-agent/backups/prune", json=valid)
        expected_digest = canonical_digest([artifacts[1]])
        assert response.status_code == 200
        assert response.json() == {
            "operation_id": OPERATION,
            "deleted_paths": ["old.enc"],
            "manifest_sha256": expected_digest,
        }
        assert not (backup_root / "old.enc").exists()
        assert (backup_root / "new.enc").exists()


def test_model_rejects_non_json_equivalents():
    valid = {
        "operation_id": OPERATION,
        "cutoff": CUTOFF,
        "expected_manifest_sha256": "0" * 64,
        "paths": [],
    }
    for field in ("operation_id", "cutoff", "expected_manifest_sha256"):
        with pytest.raises(ValidationError):
            maintenance.PruneRequest.model_validate(dict(valid, **{field: 7}))
