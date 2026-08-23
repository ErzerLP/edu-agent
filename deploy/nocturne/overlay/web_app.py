# pyright: reportMissingImports=false

from __future__ import annotations

import importlib.util
import json
import logging
import os
import re
import secrets
import urllib.error
import urllib.request
from contextlib import asynccontextmanager
from datetime import datetime, timedelta, timezone
from urllib.parse import parse_qs, urlsplit
from uuid import NAMESPACE_URL, uuid5

from fastapi import FastAPI
from fastapi.responses import JSONResponse
from pydantic import ConfigDict
from sqlalchemy import DateTime, Integer, MetaData, String, delete, func, select
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column
from starlette.applications import Starlette
from starlette.routing import Mount, Route
from starlette.types import ASGIApp, Receive, Scope, Send

import config as _cfg
from api.browse import CreateMemoryRequest, NodeUpdate, router as browse_router
from api.maintenance import router as maintenance_router
from db import close_db, get_db_manager, get_preset_service
from edu_agent_maintenance import router as internal_router, validate_startup_secrets, validated_inventory
from health import health_check, router as health_router
from locales.middleware import LocaleMiddleware
from namespace_middleware import NamespaceMiddleware

for model in (CreateMemoryRequest, NodeUpdate):
    model.model_config = ConfigDict(extra="forbid")
    model.model_rebuild(force=True)

_EXACT = {
    ("GET", "/health"): set(),
    ("GET", "/api/health"): set(),
    ("GET", "/api/browse/node"): {"domain", "path"},
    ("POST", "/api/browse/node"): set(),
    ("PUT", "/api/browse/node"): {"domain", "path"},
    ("DELETE", "/api/browse/node"): {"domain", "path"},
    ("GET", "/api/browse/search"): {"q", "domain", "limit"},
    ("GET", "/api/maintenance/orphans"): set(),
    ("GET", "/internal/edu-agent/capabilities"): set(),
    ("GET", "/internal/edu-agent/backups"): set(),
    ("POST", "/internal/edu-agent/backups/prune"): set(),
}
_PATTERNS = [
    (re.compile(r"^/api/maintenance/orphans/[1-9][0-9]*$"), {"GET", "DELETE"}, set()),
    (re.compile(r"^/internal/edu-agent/nodes/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/references$"), {"GET"}, set()),
    (re.compile(r"^/internal/edu-agent/nodes/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/review-reference$"), {"DELETE"}, set()),
]
_MIGRATION_BACKUP_MAX_AGE = timedelta(minutes=10)
_MIGRATION_BACKUP_FUTURE_SKEW = timedelta(minutes=1)
_UTC_ARTIFACT_TIMESTAMP = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?Z$")


class ProductionBoundary:
    def __init__(self, app: ASGIApp, api_token: str, maintenance_token: str):
        self.app = app
        self.api_token = api_token
        self.maintenance_token = maintenance_token

    async def __call__(self, scope: Scope, receive: Receive, send: Send):
        if scope.get("type") == "lifespan":
            await self.app(scope, receive, send)
            return
        if scope.get("type") != "http":
            await JSONResponse({"detail": "Not Found"}, status_code=404)(scope, receive, send)
            return
        method, path = scope.get("method", ""), scope.get("path", "")
        allowed_query = _EXACT.get((method, path))
        if allowed_query is None:
            for pattern, methods, query_names in _PATTERNS:
                if pattern.fullmatch(path) and method in methods:
                    allowed_query = query_names
                    break
        if allowed_query is None:
            await JSONResponse({"detail": "Not Found"}, status_code=404)(scope, receive, send)
            return
        query = parse_qs(scope.get("query_string", b"").decode("ascii", "strict"), keep_blank_values=True)
        if not set(query).issubset(allowed_query):
            await JSONResponse({"detail": "unexpected query parameter"}, status_code=422)(scope, receive, send)
            return
        if path not in ("/health", "/api/health"):
            expected = self.maintenance_token if path.startswith("/internal/") else self.api_token
            headers = {key.lower(): value for key, value in scope.get("headers", [])}
            authorization = headers.get(b"authorization", b"").decode("latin-1")
            provided = authorization[7:] if authorization.startswith("Bearer ") else ""
            if not provided or not secrets.compare_digest(provided, expected):
                await JSONResponse({"detail": "Unauthorized"}, status_code=401)(scope, receive, send)
                return
        await self.app(scope, receive, send)


class _MigrationBase(DeclarativeBase):
    pass


class _SchemaMigration(_MigrationBase):
    __tablename__ = "schema_migrations"

    version: Mapped[str] = mapped_column(String, primary_key=True)
    applied_at: Mapped[object] = mapped_column(DateTime, server_default=func.current_timestamp())


class _MigrationLeaseState(_MigrationBase):
    __tablename__ = "edu_agent_migration_lease_state"

    singleton_id: Mapped[int] = mapped_column(Integer, primary_key=True)
    operation_id: Mapped[str] = mapped_column(String, nullable=False)
    backup_identity: Mapped[str] = mapped_column(String, nullable=False)


async def _has_application_data(engine) -> bool:
    metadata = MetaData()
    async with engine.connect() as connection:
        await connection.run_sync(metadata.reflect)
        for table in sorted(metadata.tables.values(), key=lambda value: value.name):
            if table.name == "schema_migrations":
                continue
            count = await connection.scalar(select(func.count()).select_from(table))
            if count:
                return True
    return False


def _as_utc(value: object) -> datetime:
    if not isinstance(value, datetime):
        raise RuntimeError("schema migration timestamp is unavailable")
    if value.tzinfo is None:
        return value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc)


def _artifact_created_at(value: object) -> datetime:
    if not isinstance(value, str) or not _UTC_ARTIFACT_TIMESTAMP.fullmatch(value):
        raise RuntimeError("managed encrypted migration backup timestamp is not canonical UTC")
    try:
        parsed = datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as exc:
        raise RuntimeError("managed encrypted migration backup timestamp is invalid") from exc
    return parsed.astimezone(timezone.utc)


def _require_encrypted_managed_backup(*, not_before: datetime, checked_at: datetime) -> str:
    try:
        _, artifacts, manifest_digest = validated_inventory()
    except Exception as exc:
        raise RuntimeError("managed encrypted migration backup inventory is unavailable") from exc
    if not artifacts or not re.fullmatch(r"[0-9a-f]{64}", manifest_digest):
        raise RuntimeError("managed encrypted migration backup is required before upgrading an existing database")
    try:
        latest = max(_artifact_created_at(item.get("created_at")) for item in artifacts if isinstance(item, dict))
    except (RuntimeError, ValueError) as exc:
        raise RuntimeError("managed encrypted migration backup inventory has no valid UTC artifact") from exc
    if latest < not_before or latest > checked_at + _MIGRATION_BACKUP_FUTURE_SKEW:
        raise RuntimeError("managed encrypted migration backup is not fresh for this schema upgrade")
    logging.getLogger(__name__).info("validated fresh managed encrypted migration backup before schema upgrade")
    return manifest_digest


def _migration_operation_id(migration_files: list[str]) -> str:
    target = "edu-agent-nocturne-migration-v1\n" + "\n".join(migration_files)
    return str(uuid5(NAMESPACE_URL, target))


def _migration_lease_request(action: str, operation_id: str, backup_identity: str) -> None:
    base_url = os.environ.get("EDU_AGENT_SERVER_INTERNAL_URL", "").strip()
    token = os.environ.get("EDU_AGENT_SERVER_MAINTENANCE_TOKEN", "")
    local_token = os.environ.get("EDU_AGENT_MAINTENANCE_TOKEN", "")
    parsed = urlsplit(base_url)
    if (
        action not in {"acquire", "release"}
        or parsed.scheme not in {"http", "https"}
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
        or not token
        or not local_token
        or not secrets.compare_digest(token, local_token)
        or not re.fullmatch(r"[0-9a-f]{64}", backup_identity)
    ):
        raise RuntimeError("edu-agent migration lease configuration is invalid")
    request = urllib.request.Request(
        base_url.rstrip("/") + f"/internal/privacy/migrations/{action}",
        data=json.dumps({
            "operation_id": operation_id,
            "backup_identity": backup_identity,
        }, sort_keys=True, separators=(",", ":")).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            status = response.status
            body = response.read()
    except (OSError, urllib.error.HTTPError, urllib.error.URLError) as exc:
        raise RuntimeError("edu-agent migration lease service is unavailable") from exc
    if action == "release":
        if status != 204 or body:
            raise RuntimeError("edu-agent migration lease release response is invalid")
        return
    try:
        payload = json.loads(body)
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise RuntimeError("edu-agent migration lease acquire response is invalid") from exc
    if (
        status != 200
        or not isinstance(payload, dict)
        or set(payload) != {"operation_id", "status", "replayed"}
        or payload.get("operation_id") != operation_id
        or payload.get("status") != "acquired"
        or not isinstance(payload.get("replayed"), bool)
    ):
        raise RuntimeError("edu-agent migration lease acquire response is invalid")


async def _managed_run_migrations(engine) -> None:
    upgrade_checked_at = datetime.now(timezone.utc)
    async with engine.begin() as connection:
        await connection.run_sync(_MigrationBase.metadata.create_all)
    async with AsyncSession(engine) as session:
        applied = await session.scalars(select(_SchemaMigration.version).order_by(_SchemaMigration.version))
        applied_versions = set(applied.all())
        latest_applied_at = await session.scalar(select(func.max(_SchemaMigration.applied_at)))

    migrations_dir = os.path.join(os.path.dirname(__file__), "db", "migrations")
    try:
        directory_entries = os.listdir(migrations_dir)
    except OSError as exc:
        raise RuntimeError("migration directory is unavailable") from exc
    migration_files = sorted(
        name for name in directory_entries if name.endswith(".py") and name[0].isdigit()
    )
    pending = [name for name in migration_files if name not in applied_versions]
    async with AsyncSession(engine) as session:
        lease_state = await session.get(_MigrationLeaseState, 1)
        stored_lease = None if lease_state is None else (lease_state.operation_id, lease_state.backup_identity)
    if not pending:
        if stored_lease is not None:
            _migration_lease_request("release", stored_lease[0], stored_lease[1])
            async with AsyncSession(engine) as session:
                async with session.begin():
                    await session.execute(delete(_MigrationLeaseState).where(_MigrationLeaseState.singleton_id == 1))
        return

    acquired_lease: tuple[str, str] | None = None
    if await _has_application_data(engine):
        not_before = upgrade_checked_at - _MIGRATION_BACKUP_MAX_AGE
        if latest_applied_at is not None:
            not_before = max(not_before, _as_utc(latest_applied_at))
        backup_identity = _require_encrypted_managed_backup(not_before=not_before, checked_at=upgrade_checked_at)
        operation_id = _migration_operation_id(migration_files)
        if stored_lease is not None and stored_lease != (operation_id, backup_identity):
            raise RuntimeError("persisted migration lease identity does not match the pending migration")
        _migration_lease_request("acquire", operation_id, backup_identity)
        acquired_lease = (operation_id, backup_identity)
        if stored_lease is None:
            async with AsyncSession(engine) as session:
                async with session.begin():
                    session.add(_MigrationLeaseState(
                        singleton_id=1,
                        operation_id=operation_id,
                        backup_identity=backup_identity,
                    ))
    else:
        logging.getLogger(__name__).info("initializing fresh database without a pre-migration backup")

    for filename in pending:
        safe_stem = filename[:-3].replace(".", "_")
        module_name = f"db.migrations.{safe_stem}"
        spec = importlib.util.spec_from_file_location(module_name, os.path.join(migrations_dir, filename))
        if spec is None or spec.loader is None:
            raise RuntimeError("migration module could not be loaded")
        module = importlib.util.module_from_spec(spec)
        module.__package__ = "db.migrations"
        spec.loader.exec_module(module)
        if hasattr(module, "up"):
            await module.up(engine)
        else:
            logging.getLogger(__name__).warning("migration %s has no up function", filename)
        async with AsyncSession(engine) as session:
            async with session.begin():
                session.add(_SchemaMigration(version=filename))
    if acquired_lease is not None:
        _migration_lease_request("release", acquired_lease[0], acquired_lease[1])
        async with AsyncSession(engine) as session:
            async with session.begin():
                await session.execute(delete(_MigrationLeaseState).where(_MigrationLeaseState.singleton_id == 1))
    logging.getLogger(__name__).info("successfully applied pending migrations")


@asynccontextmanager
async def default_lifespan(app: Starlette):
    _cfg.ensure_config_exists()
    import db.migrations.runner as migration_runner
    migration_runner.run_migrations = _managed_run_migrations
    db = get_db_manager()
    await db.init_db()
    await get_preset_service().auto_promote_from_config()
    yield
    await close_db()


def build_web_app(*, extra_routes=None, extra_prefixes=None, lifespan=None):
    if extra_routes or extra_prefixes:
        raise RuntimeError("production image does not expose extension transports")
    api_token, maintenance_token = validate_startup_secrets()
    api = FastAPI(title="Nocturne Memory API", version="2.5.6", docs_url=None, redoc_url=None, openapi_url=None)
    api.include_router(health_router)
    api.include_router(browse_router)
    api.include_router(maintenance_router)
    api_app = NamespaceMiddleware(LocaleMiddleware(api))

    internal = FastAPI(title="edu-agent-maintenance-v1", docs_url=None, redoc_url=None, openapi_url=None)
    internal.include_router(internal_router)

    async def root_health(request):
        return await health_check()

    inner = Starlette(
        routes=[Mount("/api", app=api_app), Mount("/internal", app=internal), Route("/health", endpoint=root_health)],
        lifespan=lifespan or default_lifespan,
    )
    return ProductionBoundary(inner, api_token, maintenance_token)
