#!/usr/bin/env python3
from __future__ import annotations

import asyncio
import importlib
import os
from urllib.parse import unquote, urlsplit


async def migrate() -> None:
    database_url = os.environ["DATABASE_URL"].replace("postgresql+asyncpg://", "postgresql://", 1)
    parsed = urlsplit(database_url)
    fixture_digest = os.environ["EDU_AGENT_FAILED_FORWARD_FIXTURE_SHA256"]
    base_digest = os.environ["EDU_AGENT_FAILED_FORWARD_BASE_DIGEST"]
    asyncpg = importlib.import_module("asyncpg")
    connection = await asyncpg.connect(
        host=parsed.hostname,
        port=parsed.port or 5432,
        user=unquote(parsed.username or ""),
        password=unquote(parsed.password or ""),
        database=parsed.path.lstrip("/"),
        ssl=False,
    )
    try:
        async with connection.transaction():
            nodes = await connection.fetchval("SELECT to_regclass('public.nodes')::text")
            renamed = await connection.fetchval("SELECT to_regclass('public.nodes_pre_a84_failed_forward')::text")
            if nodes != "nodes" or renamed is not None:
                raise RuntimeError("failed-forward fixture requires the pre-upgrade nodes table")
            execute = getattr(connection, "execute")
            await execute("ALTER TABLE nodes RENAME TO nodes_pre_a84_failed_forward")
            await execute("UPDATE memories SET content='failed-forward incompatible content'")
            await execute("UPDATE search_documents SET content='failed-forward incompatible content'")
            await execute(
                """
                CREATE TABLE edu_agent_failed_forward_release(
                    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
                    fixture_sha256 TEXT NOT NULL CHECK(length(fixture_sha256)=64),
                    base_platform_digest TEXT NOT NULL CHECK(base_platform_digest ~ '^sha256:[0-9a-f]{64}$'),
                    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
                )
                """
            )
            await execute(
                "INSERT INTO edu_agent_failed_forward_release(singleton,fixture_sha256,base_platform_digest) VALUES(TRUE,$1,$2)",
                fixture_digest,
                base_digest,
            )
    finally:
        await connection.close()


if __name__ == "__main__":
    asyncio.run(migrate())
    raise SystemExit(42)
