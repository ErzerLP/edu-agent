# pyright: reportMissingImports=false

from __future__ import annotations

import fcntl
import hashlib
import json
import os
import re
import secrets
import stat
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath
from urllib.parse import quote
from uuid import UUID, uuid4

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, ConfigDict, field_validator
from sqlalchemy import or_, select

from db import get_changeset_store, get_db_manager
from db.models import Edge, GlossaryKeyword, Memory, MemoryAccessLog, Node, Path as PathModel, Preset, SearchDocument

UPSTREAM_COMMIT = "54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254"
COMPAT_REVISION = "edu-agent-maintenance-v1"
BOOT_EPOCH = str(uuid4())
INVENTORY_FORMAT = "edu-agent-managed-backup-inventory-v1"
INVENTORY_NAME = "managed-inventory.json"
INVENTORY_LOCK_NAME = ".edu-agent-backup.lock"
PRUNE_JOURNAL_FORMAT = "edu-agent-backup-prune-journal-v1"
PRUNE_JOURNAL_NAME = ".edu-agent-backup-prune.json"
PRUNE_JOURNAL_TEMP_NAME = ".edu-agent-backup-prune.json.tmp"
MANIFEST_TEMP_NAME = ".edu-agent-backup-manifest.next"
QUARANTINE_PREFIX = ".edu-agent-backup-quarantine."
_MANIFEST_LIMIT = 16 * 1024 * 1024
_JOURNAL_LIMIT = 40 * 1024 * 1024
_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
_UTC_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$")


class InventoryError(RuntimeError):
    pass


class PruneRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)

    operation_id: str
    cutoff: str
    expected_manifest_sha256: str
    paths: list[str]

    @field_validator("operation_id")
    @classmethod
    def validate_operation_id(cls, value: str) -> str:
        try:
            parsed = UUID(value)
        except (ValueError, AttributeError) as exc:
            raise ValueError("canonical operation UUID is required") from exc
        if str(parsed) != value or parsed.int == 0:
            raise ValueError("canonical operation UUID is required")
        return value

    @field_validator("cutoff")
    @classmethod
    def validate_cutoff(cls, value: str) -> str:
        try:
            parse_utc(value)
        except InventoryError as exc:
            raise ValueError(str(exc)) from exc
        return value

    @field_validator("expected_manifest_sha256")
    @classmethod
    def validate_manifest_digest(cls, value: str) -> str:
        if not _SHA256_RE.fullmatch(value):
            raise ValueError("manifest SHA-256 is required")
        return value

    @field_validator("paths")
    @classmethod
    def validate_paths(cls, value: list[str]) -> list[str]:
        if value != sorted(value) or len(value) != len(set(value)):
            raise ValueError("backup paths must be sorted and unique")
        try:
            for path in value:
                _artifact_name(path)
        except InventoryError as exc:
            raise ValueError(str(exc)) from exc
        return value


def validate_startup_secrets(api_token: str | None = None, maintenance_token: str | None = None) -> tuple[str, str]:
    if api_token is None:
        import config
        api_token = config.get("api_token")
    if maintenance_token is None:
        maintenance_token = os.environ.get("EDU_AGENT_MAINTENANCE_TOKEN")
    if not isinstance(api_token, str) or len(api_token) < 32 or api_token.strip() != api_token:
        raise RuntimeError("API_TOKEN must contain at least 32 serialized characters")
    if not isinstance(maintenance_token, str) or len(maintenance_token) < 32 or len(maintenance_token.encode("utf-8")) < 32 or maintenance_token.strip() != maintenance_token:
        raise RuntimeError("EDU_AGENT_MAINTENANCE_TOKEN must contain at least 256 bits and 32 serialized characters")
    if secrets.compare_digest(api_token, maintenance_token):
        raise RuntimeError("API and maintenance tokens must differ")
    return api_token, maintenance_token


def canonical_uuid(value: str) -> str:
    try:
        parsed = UUID(value)
    except (ValueError, AttributeError) as exc:
        raise HTTPException(status_code=422, detail="canonical node UUID is required") from exc
    if str(parsed) != value or parsed.int == 0:
        raise HTTPException(status_code=422, detail="canonical node UUID is required")
    return value


def parse_utc(value: str) -> datetime:
    if not isinstance(value, str) or not _UTC_RE.fullmatch(value):
        raise InventoryError("canonical UTC timestamp is required")
    try:
        parsed = datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as exc:
        raise InventoryError("canonical UTC timestamp is required") from exc
    return parsed.astimezone(timezone.utc)


def _utc_sort_key(value: str) -> tuple[str, str]:
    timestamp = value[:-1]
    if "." not in timestamp:
        return timestamp, "000000000"
    whole, fraction = timestamp.split(".", 1)
    return whole, fraction.ljust(9, "0")


def _review_references(node_uuid: str, memory_ids: set[int], edge_ids: set[int]) -> list[str]:
    rows, _ = get_changeset_store().get_snapshot_view()
    refs: list[str] = []
    for key, entry in rows.items():
        table = entry.get("table")
        relevant = False
        for row in (entry.get("before"), entry.get("after")):
            if not isinstance(row, dict):
                continue
            if any(row.get(field) == node_uuid for field in ("uuid", "node_uuid", "parent_uuid", "child_uuid")):
                relevant = True
            if table == "memories" and row.get("id") in memory_ids:
                relevant = True
            if table == "edges" and row.get("id") in edge_ids:
                relevant = True
            if table == "paths" and row.get("edge_id") in edge_ids:
                relevant = True
        if relevant:
            refs.append(key)
    return sorted(set(refs))


async def enumerate_references(node_uuid: str) -> dict:
    db = get_db_manager()
    async with db.session() as session:
        if await session.get(Node, node_uuid) is None:
            raise HTTPException(status_code=404, detail="node not found")
        memories = list((await session.execute(select(Memory).where(Memory.node_uuid == node_uuid).order_by(Memory.id))).scalars())
        if any(not isinstance(item.id, int) or item.id <= 0 for item in memories):
            raise HTTPException(status_code=409, detail="invalid memory identifier")
        active = [item.id for item in memories if not item.deprecated]
        if len(active) > 1:
            raise HTTPException(status_code=409, detail="multiple active memory versions")
        edges = list((await session.execute(select(Edge).where(or_(Edge.parent_uuid == node_uuid, Edge.child_uuid == node_uuid)).order_by(Edge.id))).scalars())
        incoming = {item.id for item in edges if item.child_uuid == node_uuid}
        path_rows = list((await session.execute(select(PathModel).where(or_(PathModel.node_uuid == node_uuid, PathModel.edge_id.in_(incoming))).order_by(PathModel.namespace, PathModel.domain, PathModel.path))).scalars())
        seen_edges: set[int] = set()
        paths = []
        for item in path_rows:
            alias = item.edge_id in seen_edges
            seen_edges.add(item.edge_id)
            paths.append({"namespace": item.namespace, "domain": item.domain, "path": item.path, "uri": f"{item.domain}://{item.path}", "alias": alias})
        glossary = [row[0] for row in (await session.execute(select(GlossaryKeyword.keyword).where(GlossaryKeyword.node_uuid == node_uuid).order_by(GlossaryKeyword.keyword))).all()]
        searches = list((await session.execute(select(SearchDocument).where(SearchDocument.node_uuid == node_uuid).order_by(SearchDocument.namespace, SearchDocument.domain, SearchDocument.path))).scalars())
        logs = [str(row[0]) for row in (await session.execute(select(MemoryAccessLog.id).where(MemoryAccessLog.node_uuid == node_uuid).order_by(MemoryAccessLog.id))).all()]
        path_keys = {(item["namespace"], item["uri"]) for item in paths}
        presets = list((await session.execute(select(Preset).order_by(Preset.name))).scalars())
        boot_uris = []
        for preset in presets:
            try:
                mapping = json.loads(preset.boot_uris)
            except (TypeError, json.JSONDecodeError) as exc:
                raise HTTPException(status_code=409, detail="invalid boot preset") from exc
            if not isinstance(mapping, dict):
                raise HTTPException(status_code=409, detail="invalid boot preset")
            for namespace, values in sorted(mapping.items()):
                if not isinstance(namespace, str) or not isinstance(values, list) or any(not isinstance(value, str) for value in values):
                    raise HTTPException(status_code=409, detail="invalid boot preset")
                for uri in sorted(value for value in values if (namespace, value) in path_keys):
                    boot_uris.append({"preset": preset.name, "namespace": namespace, "uri": uri})
    memory_ids = {item.id for item in memories}
    edge_ids = {item.id for item in edges}
    return {
        "node_uuid": node_uuid,
        "complete": True,
        "active_memory_id": active[0] if active else 0,
        "memory_ids": sorted(memory_ids),
        "paths": paths,
        "edge_ids": [str(value) for value in sorted(edge_ids)],
        "glossary_keywords": sorted(set(glossary)),
        "search_document_ids": [f"{quote(item.namespace, safe='')}|{item.domain}://{item.path}" for item in searches],
        "access_log_ids": logs,
        "boot_uris": boot_uris,
        "review_references": _review_references(node_uuid, memory_ids, edge_ids),
    }


@dataclass(frozen=True)
class _FileSnapshot:
    device: int
    inode: int
    size: int
    sha256: str


@dataclass
class _RootHandle:
    path: Path
    fd: int
    identities: tuple[tuple[int, int], ...]
    lock_fd: int = -1
    lock_identity: tuple[int, int, int] | None = None

    def close(self) -> None:
        os.close(self.fd)


def _open_directory_chain(path: Path) -> tuple[int, tuple[tuple[int, int], ...]]:
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW
    fd = os.open("/", flags)
    identities = [(os.fstat(fd).st_dev, os.fstat(fd).st_ino)]
    try:
        for part in path.parts[1:]:
            if part in ("", ".", ".."):
                raise InventoryError("backup root must use a canonical absolute path")
            next_fd = os.open(part, flags, dir_fd=fd)
            os.close(fd)
            fd = next_fd
            info = os.fstat(fd)
            if not stat.S_ISDIR(info.st_mode):
                raise InventoryError("backup root must be an existing directory")
            identities.append((info.st_dev, info.st_ino))
    except (OSError, InventoryError) as exc:
        os.close(fd)
        if isinstance(exc, InventoryError):
            raise
        raise InventoryError("backup root and its parents must be existing non-symlink directories") from exc
    return fd, tuple(identities)


def _backup_root() -> _RootHandle:
    raw = os.environ.get("EDU_AGENT_BACKUP_ROOT", "/app/backups")
    root = Path(raw)
    if not root.is_absolute() or raw != os.path.normpath(raw):
        raise InventoryError("backup root must use a canonical absolute path")
    try:
        fd, identities = _open_directory_chain(root)
    except (OSError, InventoryError) as exc:
        if isinstance(exc, InventoryError):
            raise
        raise InventoryError("backup root must be an existing absolute non-symlink directory") from exc
    return _RootHandle(root, fd, identities)


def _assert_root_unchanged(root: _RootHandle) -> None:
    try:
        check_fd, identities = _open_directory_chain(root.path)
    except (OSError, InventoryError) as exc:
        raise InventoryError("backup root or a parent directory changed") from exc
    try:
        if identities != root.identities:
            raise InventoryError("backup root or a parent directory changed")
    finally:
        os.close(check_fd)


def _same_identity(left: os.stat_result, right: os.stat_result) -> bool:
    return (left.st_dev, left.st_ino, left.st_size) == (right.st_dev, right.st_ino, right.st_size)


def _artifact_name(raw: str) -> str:
    if (
        not isinstance(raw, str)
        or not raw
        or raw in (INVENTORY_NAME, INVENTORY_LOCK_NAME, ".", "..")
        or raw.startswith(".edu-agent-backup-")
        or "/" in raw
        or "\\" in raw
        or "\x00" in raw
        or any(ord(character) < 0x20 or ord(character) == 0x7F for character in raw)
        or PurePosixPath(raw).name != raw
    ):
        raise InventoryError("backup artifacts must use flat immutable filenames")
    return raw


def _open_regular(root: _RootHandle, name: str, label: str) -> int:
    flags = os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK
    try:
        fd = os.open(name, flags, dir_fd=root.fd)
    except OSError as exc:
        raise InventoryError(f"{label} must be an existing non-symlink regular file") from exc
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        os.close(fd)
        raise InventoryError(f"{label} must be a single-link regular file")
    return fd


def _hash_fd(fd: int) -> str:
    digest = hashlib.sha256()
    os.lseek(fd, 0, os.SEEK_SET)
    for chunk in iter(lambda: os.read(fd, 1024 * 1024), b""):
        digest.update(chunk)
    return digest.hexdigest()


def _snapshot_fd(fd: int, label: str) -> _FileSnapshot:
    before = os.fstat(fd)
    digest = _hash_fd(fd)
    after = os.fstat(fd)
    if not _same_identity(before, after) or (before.st_mtime_ns, before.st_ctime_ns) != (after.st_mtime_ns, after.st_ctime_ns):
        raise InventoryError(f"{label} changed while it was being validated")
    return _FileSnapshot(after.st_dev, after.st_ino, after.st_size, digest)


def _decode_json(raw: bytes, label: str) -> object:
    def reject_duplicates(pairs: list[tuple[str, object]]) -> dict:
        result: dict[str, object] = {}
        for key, value in pairs:
            if key in result:
                raise InventoryError(f"{label} contains duplicate JSON keys")
            result[key] = value
        return result

    try:
        return json.loads(raw.decode("utf-8"), object_pairs_hook=reject_duplicates)
    except InventoryError:
        raise
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise InventoryError(f"{label} is not valid JSON") from exc


def _read_regular_bytes(root: _RootHandle, name: str, label: str, limit: int) -> tuple[bytes, _FileSnapshot]:
    fd = _open_regular(root, name, label)
    try:
        snapshot = _snapshot_fd(fd, label)
        if snapshot.size > limit:
            raise InventoryError(f"{label} is too large")
        os.lseek(fd, 0, os.SEEK_SET)
        raw = b"".join(iter(lambda: os.read(fd, 1024 * 1024), b""))
        after = os.fstat(fd)
        if (
            len(raw) != snapshot.size
            or hashlib.sha256(raw).hexdigest() != snapshot.sha256
            or (after.st_dev, after.st_ino, after.st_size) != (snapshot.device, snapshot.inode, snapshot.size)
        ):
            raise InventoryError(f"{label} changed while it was being read")
        return raw, snapshot
    finally:
        os.close(fd)


def _read_manifest(root: _RootHandle) -> tuple[dict | None, _FileSnapshot | None]:
    try:
        raw, snapshot = _read_regular_bytes(root, INVENTORY_NAME, "inventory", _MANIFEST_LIMIT)
    except InventoryError as exc:
        try:
            os.stat(INVENTORY_NAME, dir_fd=root.fd, follow_symlinks=False)
        except FileNotFoundError:
            return None, None
        raise exc
    data = _decode_json(raw, "inventory")
    if not isinstance(data, dict):
        raise InventoryError("inventory shape is invalid")
    return data, snapshot


def _validate_manifest_data(data: object) -> list[dict]:
    if not isinstance(data, dict) or set(data) != {"format", "artifacts"} or data.get("format") != INVENTORY_FORMAT or not isinstance(data.get("artifacts"), list):
        raise InventoryError("inventory shape is invalid")
    result: list[dict] = []
    seen: set[str] = set()
    required = {"path", "created_at", "size", "sha256", "learner_generation", "wrapped_key_id"}
    for item in data["artifacts"]:
        if not isinstance(item, dict) or set(item) != required:
            raise InventoryError("inventory artifact shape is invalid")
        name = _artifact_name(item["path"])
        if name in seen:
            raise InventoryError("inventory artifact shape is invalid")
        try:
            wrapped_key_id = UUID(item["wrapped_key_id"])
        except (ValueError, TypeError, AttributeError) as exc:
            raise InventoryError("wrapped key ID must be a UUID") from exc
        if (
            str(wrapped_key_id) != item["wrapped_key_id"]
            or not isinstance(item["size"], int)
            or isinstance(item["size"], bool)
            or item["size"] < 0
            or not isinstance(item["learner_generation"], int)
            or isinstance(item["learner_generation"], bool)
            or item["learner_generation"] < 1
            or not isinstance(item["sha256"], str)
            or not _SHA256_RE.fullmatch(item["sha256"])
        ):
            raise InventoryError("inventory artifact metadata is invalid")
        parse_utc(item["created_at"])
        seen.add(name)
        result.append(dict(item))
    result.sort(key=lambda value: (_utc_sort_key(value["created_at"]), value["path"]))
    return result


def _manifest_document(artifacts: list[dict]) -> dict:
    ordered = sorted((dict(item) for item in artifacts), key=lambda value: (_utc_sort_key(value["created_at"]), value["path"]))
    return {"format": INVENTORY_FORMAT, "artifacts": ordered}


def _manifest_payload(artifacts: list[dict]) -> bytes:
    return json.dumps(_manifest_document(artifacts), sort_keys=True, separators=(",", ":")).encode("utf-8") + b"\n"


def _manifest_sha256(artifacts: list[dict]) -> str:
    return hashlib.sha256(_manifest_payload(artifacts)).hexdigest()


def _current_snapshot(root: _RootHandle, name: str, label: str) -> _FileSnapshot:
    fd = _open_regular(root, name, label)
    try:
        return _snapshot_fd(fd, label)
    finally:
        os.close(fd)


def _assert_manifest_unchanged(root: _RootHandle, expected: _FileSnapshot | None) -> None:
    if expected is None:
        try:
            os.stat(INVENTORY_NAME, dir_fd=root.fd, follow_symlinks=False)
        except FileNotFoundError:
            return
        raise InventoryError("inventory changed during the operation")
    try:
        current = _current_snapshot(root, INVENTORY_NAME, "inventory")
    except InventoryError as exc:
        raise InventoryError("inventory changed during the operation") from exc
    if current != expected:
        raise InventoryError("inventory changed during the operation")


def _assert_lock_unchanged(root: _RootHandle) -> None:
    if root.lock_fd < 0 or root.lock_identity is None:
        raise InventoryError("backup inventory lock is not held")
    held = os.fstat(root.lock_fd)
    try:
        named = os.stat(INVENTORY_LOCK_NAME, dir_fd=root.fd, follow_symlinks=False)
    except OSError as exc:
        raise InventoryError("backup inventory lock changed during the operation") from exc
    identity = (held.st_dev, held.st_ino, held.st_nlink)
    named_identity = (named.st_dev, named.st_ino, named.st_nlink)
    if (
        not stat.S_ISREG(held.st_mode)
        or not stat.S_ISREG(named.st_mode)
        or identity != root.lock_identity
        or named_identity != root.lock_identity
        or held.st_nlink != 1
    ):
        raise InventoryError("backup inventory lock changed during the operation")


@contextmanager
def _locked_backup_root(*, exclusive: bool):
    root = _backup_root()
    lock_fd = -1
    try:
        flags = os.O_RDWR | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW
        try:
            lock_fd = os.open(INVENTORY_LOCK_NAME, flags, 0o600, dir_fd=root.fd)
        except OSError as exc:
            raise InventoryError("backup inventory lock must be a regular file") from exc
        lock_info = os.fstat(lock_fd)
        if not stat.S_ISREG(lock_info.st_mode) or lock_info.st_nlink != 1:
            raise InventoryError("backup inventory lock must be a single-link regular file")
        fcntl.flock(lock_fd, fcntl.LOCK_EX if exclusive else fcntl.LOCK_SH)
        named_info = os.stat(INVENTORY_LOCK_NAME, dir_fd=root.fd, follow_symlinks=False)
        if not _same_identity(lock_info, named_info):
            raise InventoryError("backup inventory lock changed")
        root.lock_fd = lock_fd
        root.lock_identity = (lock_info.st_dev, lock_info.st_ino, lock_info.st_nlink)
        _assert_lock_unchanged(root)
        _assert_root_unchanged(root)
        yield root
        _assert_lock_unchanged(root)
        _assert_root_unchanged(root)
    except OSError as exc:
        raise InventoryError("backup root changed during the operation") from exc
    finally:
        if lock_fd >= 0:
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
            finally:
                os.close(lock_fd)
        root.close()


def _entry_snapshot(root: _RootHandle, name: str, label: str) -> _FileSnapshot | None:
    try:
        os.stat(name, dir_fd=root.fd, follow_symlinks=False)
    except FileNotFoundError:
        return None
    return _current_snapshot(root, name, label)


def _assert_named_snapshot(root: _RootHandle, name: str, expected: _FileSnapshot, label: str) -> None:
    current = _entry_snapshot(root, name, label)
    if current != expected:
        raise InventoryError(f"{label} changed during the operation")


def _snapshot_record(snapshot: _FileSnapshot) -> dict:
    return {"device": snapshot.device, "inode": snapshot.inode, "size": snapshot.size, "sha256": snapshot.sha256}


def _parse_snapshot_record(value: object, label: str) -> _FileSnapshot:
    if not isinstance(value, dict) or set(value) != {"device", "inode", "size", "sha256"}:
        raise InventoryError(f"{label} binding is invalid")
    numbers = (value["device"], value["inode"], value["size"])
    if any(not isinstance(item, int) or isinstance(item, bool) or item < 0 for item in numbers) or value["inode"] == 0:
        raise InventoryError(f"{label} binding is invalid")
    if not isinstance(value["sha256"], str) or not _SHA256_RE.fullmatch(value["sha256"]):
        raise InventoryError(f"{label} binding is invalid")
    return _FileSnapshot(value["device"], value["inode"], value["size"], value["sha256"])


def _identity_record(device: int, inode: int) -> dict:
    return {"device": device, "inode": inode}


def _parse_identity_record(value: object, label: str) -> tuple[int, int]:
    if not isinstance(value, dict) or set(value) != {"device", "inode"}:
        raise InventoryError(f"{label} binding is invalid")
    device, inode = value["device"], value["inode"]
    if (
        not isinstance(device, int)
        or isinstance(device, bool)
        or device < 0
        or not isinstance(inode, int)
        or isinstance(inode, bool)
        or inode <= 0
    ):
        raise InventoryError(f"{label} binding is invalid")
    return device, inode


def _write_all(fd: int, payload: bytes, label: str) -> None:
    view = memoryview(payload)
    while view:
        written = os.write(fd, view)
        if written <= 0:
            raise InventoryError(f"failed to write {label}")
        view = view[written:]


def _create_control_file(root: _RootHandle, name: str, payload: bytes, label: str) -> _FileSnapshot:
    fd = -1
    try:
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW, 0o600, dir_fd=root.fd)
        _write_all(fd, payload, label)
        os.fsync(fd)
        os.close(fd)
        fd = -1
        os.fsync(root.fd)
        return _current_snapshot(root, name, label)
    except OSError as exc:
        raise InventoryError(f"{label} could not be persisted safely") from exc
    finally:
        if fd >= 0:
            os.close(fd)


def _remove_control(root: _RootHandle, name: str, expected: _FileSnapshot, label: str) -> None:
    _assert_named_snapshot(root, name, expected, label)
    _assert_lock_unchanged(root)
    _assert_root_unchanged(root)
    try:
        os.unlink(name, dir_fd=root.fd)
        os.fsync(root.fd)
    except OSError as exc:
        raise InventoryError(f"{label} could not be removed safely") from exc


def _cleanup_unpublished_controls(root: _RootHandle) -> None:
    for name, label in (
        (PRUNE_JOURNAL_TEMP_NAME, "prune journal temporary file"),
        (MANIFEST_TEMP_NAME, "manifest temporary file"),
    ):
        snapshot = _entry_snapshot(root, name, label)
        if snapshot is not None:
            _remove_control(root, name, snapshot, label)


def _validate_inventory(root: _RootHandle) -> tuple[list[dict], dict[str, _FileSnapshot], _FileSnapshot | None, str]:
    data, manifest_snapshot = _read_manifest(root)
    try:
        entries = set(os.listdir(root.fd))
    except OSError as exc:
        raise InventoryError("backup root could not be enumerated safely") from exc
    controls = {INVENTORY_LOCK_NAME}
    if data is None:
        if entries != controls:
            raise InventoryError("backup root contains artifacts without an inventory")
        _assert_root_unchanged(root)
        _assert_manifest_unchanged(root, None)
        return [], {}, None, _manifest_sha256([])
    result = _validate_manifest_data(data)
    snapshots: dict[str, _FileSnapshot] = {}
    for item in result:
        snapshot = _current_snapshot(root, item["path"], "backup artifact")
        if snapshot.size != item["size"] or not secrets.compare_digest(snapshot.sha256, item["sha256"]):
            raise InventoryError("backup artifact does not match inventory")
        snapshots[item["path"]] = snapshot
    try:
        entries_after = set(os.listdir(root.fd))
    except OSError as exc:
        raise InventoryError("backup root could not be enumerated safely") from exc
    expected_entries = controls | {INVENTORY_NAME} | set(snapshots)
    if entries != expected_entries or entries_after != expected_entries:
        raise InventoryError("backup root contains unregistered or missing entries")
    _assert_manifest_unchanged(root, manifest_snapshot)
    _assert_root_unchanged(root)
    return result, snapshots, manifest_snapshot, _manifest_sha256(result)


def _quarantine_name(operation_id: str, index: int) -> str:
    return f"{QUARANTINE_PREFIX}{operation_id}.{index:08d}"


def _journal_payload(value: dict) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode("utf-8") + b"\n"


def _read_prune_journal(root: _RootHandle) -> tuple[dict | None, _FileSnapshot | None]:
    snapshot = _entry_snapshot(root, PRUNE_JOURNAL_NAME, "prune journal")
    if snapshot is None:
        return None, None
    raw, current = _read_regular_bytes(root, PRUNE_JOURNAL_NAME, "prune journal", _JOURNAL_LIMIT)
    if current != snapshot:
        raise InventoryError("prune journal changed while it was being read")
    data = _decode_json(raw, "prune journal")
    if not isinstance(data, dict):
        raise InventoryError("prune journal shape is invalid")
    return data, current


def _canonical_operation_id(value: object) -> str:
    if not isinstance(value, str):
        raise InventoryError("prune journal operation ID is invalid")
    try:
        parsed = UUID(value)
    except (ValueError, AttributeError) as exc:
        raise InventoryError("prune journal operation ID is invalid") from exc
    if str(parsed) != value or parsed.int == 0:
        raise InventoryError("prune journal operation ID is invalid")
    return value


def _validate_journal(root: _RootHandle, data: dict) -> dict:
    required = {
        "format", "operation_id", "cutoff", "root", "lock", "old_manifest_sha256", "new_manifest_sha256",
        "paths", "old_manifest", "new_manifest", "old_manifest_file", "new_manifest_file",
        "artifact_bindings", "quarantine",
    }
    if set(data) != required or data.get("format") != PRUNE_JOURNAL_FORMAT:
        raise InventoryError("prune journal shape is invalid")
    operation_id = _canonical_operation_id(data["operation_id"])
    cutoff = parse_utc(data["cutoff"])
    old_digest, new_digest = data["old_manifest_sha256"], data["new_manifest_sha256"]
    if not isinstance(old_digest, str) or not _SHA256_RE.fullmatch(old_digest) or not isinstance(new_digest, str) or not _SHA256_RE.fullmatch(new_digest):
        raise InventoryError("prune journal manifest digest is invalid")
    old_artifacts = _validate_manifest_data(data["old_manifest"])
    new_artifacts = _validate_manifest_data(data["new_manifest"])
    if data["old_manifest"] != _manifest_document(old_artifacts) or data["new_manifest"] != _manifest_document(new_artifacts):
        raise InventoryError("prune journal manifest is not canonical")
    if _manifest_sha256(old_artifacts) != old_digest or _manifest_sha256(new_artifacts) != new_digest:
        raise InventoryError("prune journal manifest digest is invalid")
    paths = data["paths"]
    if not isinstance(paths, list) or not paths or any(not isinstance(path, str) for path in paths):
        raise InventoryError("prune journal paths are invalid")
    if paths != sorted(paths) or len(paths) != len(set(paths)):
        raise InventoryError("prune journal paths are invalid")
    for path in paths:
        _artifact_name(path)
    eligible = sorted(item["path"] for item in old_artifacts if parse_utc(item["created_at"]) <= cutoff)
    if paths != eligible:
        raise InventoryError("prune journal path set is not bound to its cutoff")
    path_set = set(paths)
    expected_new = [item for item in old_artifacts if item["path"] not in path_set]
    if new_artifacts != expected_new:
        raise InventoryError("prune journal manifests do not describe one exact prune")

    root_identity = _parse_identity_record(data["root"], "backup root")
    lock_identity = _parse_identity_record(data["lock"], "backup lock")
    root_info = os.fstat(root.fd)
    lock_info = os.fstat(root.lock_fd)
    if root_identity != (root_info.st_dev, root_info.st_ino) or lock_identity != (lock_info.st_dev, lock_info.st_ino):
        raise InventoryError("prune journal is bound to a different root or lock")
    _assert_lock_unchanged(root)
    _assert_root_unchanged(root)

    old_manifest_file = _parse_snapshot_record(data["old_manifest_file"], "old manifest")
    new_manifest_file = _parse_snapshot_record(data["new_manifest_file"], "new manifest")
    new_payload = _manifest_payload(new_artifacts)
    if new_manifest_file.size != len(new_payload) or not secrets.compare_digest(new_manifest_file.sha256, new_digest):
        raise InventoryError("prune journal new manifest binding is invalid")

    bindings = data["artifact_bindings"]
    if not isinstance(bindings, list) or len(bindings) != len(old_artifacts):
        raise InventoryError("prune journal artifact bindings are invalid")
    binding_map: dict[str, _FileSnapshot] = {}
    for item, binding in zip(old_artifacts, bindings, strict=True):
        if not isinstance(binding, dict) or set(binding) != {"path", "device", "inode", "size", "sha256"} or binding.get("path") != item["path"]:
            raise InventoryError("prune journal artifact bindings are invalid")
        snapshot = _parse_snapshot_record({key: binding[key] for key in ("device", "inode", "size", "sha256")}, "backup artifact")
        if snapshot.size != item["size"] or not secrets.compare_digest(snapshot.sha256, item["sha256"]):
            raise InventoryError("prune journal artifact binding does not match its manifest")
        binding_map[item["path"]] = snapshot

    quarantine = data["quarantine"]
    if not isinstance(quarantine, list) or len(quarantine) != len(paths):
        raise InventoryError("prune journal quarantine bindings are invalid")
    quarantine_map: dict[str, str] = {}
    for index, (path, binding) in enumerate(zip(paths, quarantine, strict=True)):
        if (
            not isinstance(binding, dict)
            or set(binding) != {"path", "quarantine"}
            or binding.get("path") != path
            or binding.get("quarantine") != _quarantine_name(operation_id, index)
        ):
            raise InventoryError("prune journal quarantine bindings are invalid")
        quarantine_map[path] = binding["quarantine"]

    return {
        "operation_id": operation_id,
        "old_artifacts": old_artifacts,
        "new_artifacts": new_artifacts,
        "paths": paths,
        "old_manifest": data["old_manifest"],
        "new_manifest": data["new_manifest"],
        "old_manifest_file": old_manifest_file,
        "new_manifest_file": new_manifest_file,
        "bindings": binding_map,
        "quarantine": quarantine_map,
    }


def _manifest_recovery_state(root: _RootHandle, journal: dict) -> str:
    data, snapshot = _read_manifest(root)
    if data is None or snapshot is None:
        raise InventoryError("prune recovery found a missing manifest")
    artifacts = _validate_manifest_data(data)
    document = _manifest_document(artifacts)
    if snapshot == journal["old_manifest_file"] and document == journal["old_manifest"]:
        return "old"
    if snapshot == journal["new_manifest_file"] and document == journal["new_manifest"]:
        return "new"
    raise InventoryError("prune recovery found an unexpected manifest state")


def _recovery_locations(root: _RootHandle, journal: dict, state: str) -> tuple[dict[str, str], set[str]]:
    try:
        entries = set(os.listdir(root.fd))
    except OSError as exc:
        raise InventoryError("backup root could not be enumerated during prune recovery") from exc
    allowed = {INVENTORY_LOCK_NAME, INVENTORY_NAME, PRUNE_JOURNAL_NAME}
    if PRUNE_JOURNAL_TEMP_NAME in entries:
        raise InventoryError("prune recovery found an unexpected journal temporary file")
    if state == "old":
        manifest_temp = _entry_snapshot(root, MANIFEST_TEMP_NAME, "manifest temporary file")
        if manifest_temp != journal["new_manifest_file"]:
            raise InventoryError("prune recovery manifest temporary file is missing or changed")
        allowed.add(MANIFEST_TEMP_NAME)
    elif MANIFEST_TEMP_NAME in entries:
        raise InventoryError("prune recovery found an unexpected manifest temporary file")

    locations: dict[str, str] = {}
    removed = set(journal["paths"])
    for item in journal["old_artifacts"]:
        path = item["path"]
        expected = journal["bindings"][path]
        original = _entry_snapshot(root, path, "backup artifact")
        quarantine_name = journal["quarantine"].get(path)
        quarantined = _entry_snapshot(root, quarantine_name, "quarantined backup artifact") if quarantine_name is not None else None
        if path not in removed:
            if original != expected or quarantined is not None:
                raise InventoryError("retained backup artifact changed during prune recovery")
            locations[path] = path
            allowed.add(path)
            continue
        if state == "old":
            if (original is None) == (quarantined is None):
                raise InventoryError("pruned backup artifact has an ambiguous recovery location")
            current = original if original is not None else quarantined
            if current != expected:
                raise InventoryError("pruned backup artifact changed during prune recovery")
            if original is not None:
                location = path
            elif quarantine_name is not None:
                location = quarantine_name
            else:
                raise InventoryError("prune journal is missing a quarantine name")
            locations[path] = location
            allowed.add(location)
        else:
            if original is not None:
                raise InventoryError("new manifest coexists with a removed artifact name")
            if quarantined is not None:
                if quarantined != expected or quarantine_name is None:
                    raise InventoryError("quarantined backup artifact changed during prune recovery")
                locations[path] = quarantine_name
                allowed.add(quarantine_name)
            else:
                locations[path] = ""
    if entries != allowed:
        raise InventoryError("backup root contains entries outside the prune journal")
    return locations, allowed


def _recover_prune(root: _RootHandle, data: dict, journal_snapshot: _FileSnapshot) -> None:
    journal = _validate_journal(root, data)
    state = _manifest_recovery_state(root, journal)
    locations, _ = _recovery_locations(root, journal, state)
    manifest_snapshot = journal[f"{state}_manifest_file"]
    try:
        if state == "old":
            for path in journal["paths"]:
                quarantine_name = journal["quarantine"][path]
                if locations[path] != quarantine_name:
                    continue
                _assert_named_snapshot(root, PRUNE_JOURNAL_NAME, journal_snapshot, "prune journal")
                _assert_manifest_unchanged(root, manifest_snapshot)
                _assert_lock_unchanged(root)
                _assert_root_unchanged(root)
                if _entry_snapshot(root, path, "backup artifact") is not None:
                    raise InventoryError("backup artifact name was recreated during prune recovery")
                os.rename(quarantine_name, path, src_dir_fd=root.fd, dst_dir_fd=root.fd)
                os.fsync(root.fd)
                _assert_named_snapshot(root, path, journal["bindings"][path], "backup artifact")
            _remove_control(root, MANIFEST_TEMP_NAME, journal["new_manifest_file"], "manifest temporary file")
        else:
            for path in journal["paths"]:
                quarantine_name = locations[path]
                if not quarantine_name:
                    continue
                _assert_named_snapshot(root, PRUNE_JOURNAL_NAME, journal_snapshot, "prune journal")
                _assert_manifest_unchanged(root, manifest_snapshot)
                _assert_lock_unchanged(root)
                _assert_root_unchanged(root)
                _assert_named_snapshot(root, quarantine_name, journal["bindings"][path], "quarantined backup artifact")
                os.unlink(quarantine_name, dir_fd=root.fd)
                os.fsync(root.fd)
        _assert_named_snapshot(root, PRUNE_JOURNAL_NAME, journal_snapshot, "prune journal")
        _assert_manifest_unchanged(root, manifest_snapshot)
        _remove_control(root, PRUNE_JOURNAL_NAME, journal_snapshot, "prune journal")
    except OSError as exc:
        raise InventoryError("prune recovery could not complete safely") from exc


def _recover_if_needed(root: _RootHandle) -> None:
    data, journal_snapshot = _read_prune_journal(root)
    if data is None or journal_snapshot is None:
        _cleanup_unpublished_controls(root)
        return
    _recover_prune(root, data, journal_snapshot)


def validated_inventory() -> tuple[Path, list[dict], str]:
    with _locked_backup_root(exclusive=True) as root:
        _recover_if_needed(root)
        artifacts, _, _, manifest_digest = _validate_inventory(root)
        return root.path, artifacts, manifest_digest


def _revalidate_artifact(root: _RootHandle, item: dict, expected: _FileSnapshot) -> tuple[int, _FileSnapshot]:
    fd = _open_regular(root, item["path"], "backup artifact")
    try:
        current = _snapshot_fd(fd, "backup artifact")
        if current != expected or current.size != item["size"] or not secrets.compare_digest(current.sha256, item["sha256"]):
            raise InventoryError("backup artifact changed before prune")
        return fd, current
    except Exception:
        os.close(fd)
        raise


def _crash_point(_stage: str) -> None:
    return None


def _publish_prune_journal(root: _RootHandle, journal: dict, expected_manifest: _FileSnapshot) -> _FileSnapshot:
    payload = _journal_payload(journal)
    if len(payload) > _JOURNAL_LIMIT:
        raise InventoryError("prune journal is too large")
    if _entry_snapshot(root, PRUNE_JOURNAL_NAME, "prune journal") is not None:
        raise InventoryError("a prune journal already exists")
    temp_snapshot = _create_control_file(root, PRUNE_JOURNAL_TEMP_NAME, payload, "prune journal temporary file")
    try:
        _assert_manifest_unchanged(root, expected_manifest)
        _assert_lock_unchanged(root)
        _assert_root_unchanged(root)
        _assert_named_snapshot(root, PRUNE_JOURNAL_TEMP_NAME, temp_snapshot, "prune journal temporary file")
        os.replace(PRUNE_JOURNAL_TEMP_NAME, PRUNE_JOURNAL_NAME, src_dir_fd=root.fd, dst_dir_fd=root.fd)
        _crash_point("after_journal_replace")
        os.fsync(root.fd)
        _crash_point("after_journal_fsync")
    except OSError as exc:
        raise InventoryError("prune journal could not be published safely") from exc
    journal_snapshot = _current_snapshot(root, PRUNE_JOURNAL_NAME, "prune journal")
    if journal_snapshot != temp_snapshot:
        raise InventoryError("prune journal changed while it was being published")
    return journal_snapshot


def _discard_unpublished_prune_files(root: _RootHandle) -> None:
    if _entry_snapshot(root, PRUNE_JOURNAL_NAME, "prune journal") is not None:
        return
    for name, label in (
        (PRUNE_JOURNAL_TEMP_NAME, "prune journal temporary file"),
        (MANIFEST_TEMP_NAME, "manifest temporary file"),
    ):
        snapshot = _entry_snapshot(root, name, label)
        if snapshot is None:
            continue
        try:
            _remove_control(root, name, snapshot, label)
        except InventoryError:
            return


def prune_inventory(request: PruneRequest) -> dict:
    cutoff_time = parse_utc(request.cutoff)
    with _locked_backup_root(exclusive=True) as root:
        _recover_if_needed(root)
        artifacts, snapshots, manifest_snapshot, manifest_digest = _validate_inventory(root)
        if not secrets.compare_digest(manifest_digest, request.expected_manifest_sha256):
            raise InventoryError("backup inventory digest changed")
        requested_paths = list(request.paths)
        eligible_paths = sorted(item["path"] for item in artifacts if parse_utc(item["created_at"]) <= cutoff_time)
        if requested_paths != eligible_paths:
            raise InventoryError("backup prune paths do not exactly match the eligible inventory")
        retained = [item for item in artifacts if item["path"] not in set(requested_paths)]
        new_manifest_digest = _manifest_sha256(retained)
        if not requested_paths:
            return {
                "operation_id": request.operation_id,
                "deleted_paths": [],
                "manifest_sha256": new_manifest_digest,
            }
        if manifest_snapshot is None:
            raise InventoryError("backup inventory disappeared before prune")

        items_by_path = {item["path"]: item for item in artifacts}
        for path in requested_paths:
            fd, _ = _revalidate_artifact(root, items_by_path[path], snapshots[path])
            os.close(fd)
        _assert_manifest_unchanged(root, manifest_snapshot)
        _assert_lock_unchanged(root)
        _assert_root_unchanged(root)

        manifest_temp_snapshot: _FileSnapshot | None = None
        journal_snapshot: _FileSnapshot | None = None
        try:
            manifest_temp_snapshot = _create_control_file(root, MANIFEST_TEMP_NAME, _manifest_payload(retained), "manifest temporary file")
            _assert_manifest_unchanged(root, manifest_snapshot)
            _assert_lock_unchanged(root)
            _assert_root_unchanged(root)
            root_info = os.fstat(root.fd)
            lock_info = os.fstat(root.lock_fd)
            journal = {
                "format": PRUNE_JOURNAL_FORMAT,
                "operation_id": request.operation_id,
                "cutoff": request.cutoff,
                "root": _identity_record(root_info.st_dev, root_info.st_ino),
                "lock": _identity_record(lock_info.st_dev, lock_info.st_ino),
                "old_manifest_sha256": manifest_digest,
                "new_manifest_sha256": new_manifest_digest,
                "paths": requested_paths,
                "old_manifest": _manifest_document(artifacts),
                "new_manifest": _manifest_document(retained),
                "old_manifest_file": _snapshot_record(manifest_snapshot),
                "new_manifest_file": _snapshot_record(manifest_temp_snapshot),
                "artifact_bindings": [
                    {"path": item["path"], **_snapshot_record(snapshots[item["path"]])}
                    for item in artifacts
                ],
                "quarantine": [
                    {"path": path, "quarantine": _quarantine_name(request.operation_id, index)}
                    for index, path in enumerate(requested_paths)
                ],
            }
            journal_snapshot = _publish_prune_journal(root, journal, manifest_snapshot)

            for index, path in enumerate(requested_paths):
                item = items_by_path[path]
                fd, current = _revalidate_artifact(root, item, snapshots[path])
                quarantine_name = _quarantine_name(request.operation_id, index)
                try:
                    _assert_manifest_unchanged(root, manifest_snapshot)
                    _assert_named_snapshot(root, PRUNE_JOURNAL_NAME, journal_snapshot, "prune journal")
                    _assert_lock_unchanged(root)
                    _assert_root_unchanged(root)
                    if _entry_snapshot(root, quarantine_name, "quarantined backup artifact") is not None:
                        raise InventoryError("backup quarantine name already exists")
                    named = os.stat(path, dir_fd=root.fd, follow_symlinks=False)
                    if (
                        not stat.S_ISREG(named.st_mode)
                        or named.st_nlink != 1
                        or (named.st_dev, named.st_ino, named.st_size) != (current.device, current.inode, current.size)
                    ):
                        raise InventoryError("backup artifact changed before quarantine")
                    os.rename(path, quarantine_name, src_dir_fd=root.fd, dst_dir_fd=root.fd)
                    _crash_point(f"after_quarantine_rename:{index}")
                    os.fsync(root.fd)
                    _crash_point(f"after_quarantine_fsync:{index}")
                    _assert_named_snapshot(root, quarantine_name, snapshots[path], "quarantined backup artifact")
                finally:
                    os.close(fd)

            _assert_manifest_unchanged(root, manifest_snapshot)
            _assert_named_snapshot(root, PRUNE_JOURNAL_NAME, journal_snapshot, "prune journal")
            _assert_named_snapshot(root, MANIFEST_TEMP_NAME, manifest_temp_snapshot, "manifest temporary file")
            _assert_lock_unchanged(root)
            _assert_root_unchanged(root)
            _crash_point("before_manifest_replace")
            os.replace(MANIFEST_TEMP_NAME, INVENTORY_NAME, src_dir_fd=root.fd, dst_dir_fd=root.fd)
            _crash_point("after_manifest_replace")
            os.fsync(root.fd)
            _crash_point("after_manifest_fsync")
            _assert_manifest_unchanged(root, manifest_temp_snapshot)

            for index, path in enumerate(requested_paths):
                quarantine_name = _quarantine_name(request.operation_id, index)
                _assert_named_snapshot(root, PRUNE_JOURNAL_NAME, journal_snapshot, "prune journal")
                _assert_manifest_unchanged(root, manifest_temp_snapshot)
                _assert_named_snapshot(root, quarantine_name, snapshots[path], "quarantined backup artifact")
                _assert_lock_unchanged(root)
                _assert_root_unchanged(root)
                os.unlink(quarantine_name, dir_fd=root.fd)
                _crash_point(f"after_quarantine_unlink:{index}")
                os.fsync(root.fd)
                _crash_point(f"after_quarantine_cleanup_fsync:{index}")

            _assert_named_snapshot(root, PRUNE_JOURNAL_NAME, journal_snapshot, "prune journal")
            _assert_manifest_unchanged(root, manifest_temp_snapshot)
            for item in retained:
                _assert_named_snapshot(root, item["path"], snapshots[item["path"]], "retained backup artifact")
            _assert_lock_unchanged(root)
            _assert_root_unchanged(root)
            os.unlink(PRUNE_JOURNAL_NAME, dir_fd=root.fd)
            _crash_point("after_journal_unlink")
            os.fsync(root.fd)
            _crash_point("after_journal_cleanup_fsync")
            final_artifacts, _, _, final_digest = _validate_inventory(root)
            if final_artifacts != retained or not secrets.compare_digest(final_digest, new_manifest_digest):
                raise InventoryError("backup prune final inventory does not match the committed manifest")
        except OSError as exc:
            raise InventoryError("backup prune could not be committed safely") from exc
        except Exception:
            if journal_snapshot is None:
                _discard_unpublished_prune_files(root)
            raise

        return {
            "operation_id": request.operation_id,
            "deleted_paths": requested_paths,
            "manifest_sha256": new_manifest_digest,
        }


router = APIRouter(prefix="/edu-agent", tags=["edu-agent-maintenance"])


@router.get("/capabilities")
async def capabilities():
    return {"upstream_commit": UPSTREAM_COMMIT, "compat_revision": COMPAT_REVISION, "boot_epoch": BOOT_EPOCH}


@router.get("/nodes/{node_uuid}/references")
async def references(node_uuid: str):
    return await enumerate_references(canonical_uuid(node_uuid))


@router.delete("/nodes/{node_uuid}/review-reference")
async def clear_review_reference(node_uuid: str):
    value = await enumerate_references(canonical_uuid(node_uuid))
    get_changeset_store().remove_keys(value["review_references"])
    remaining = (await enumerate_references(node_uuid))["review_references"]
    if remaining:
        raise HTTPException(status_code=409, detail="review references changed during cleanup")
    return {"success": True}


@router.get("/backups")
async def backups():
    try:
        _, artifacts, manifest_digest = validated_inventory()
    except InventoryError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    return {"validated": True, "manifest_sha256": manifest_digest, "artifacts": artifacts}


@router.post("/backups/prune")
async def prune_backups(body: PruneRequest):
    try:
        return prune_inventory(body)
    except InventoryError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
