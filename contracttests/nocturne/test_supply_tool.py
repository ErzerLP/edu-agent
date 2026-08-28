# pyright: reportMissingImports=false

from __future__ import annotations

import hashlib
import importlib.util
import json
import shutil
import stat
import subprocess
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
TOOL_PATH = REPO / "deploy/nocturne/scripts/tool.py"


def load_tool():
    spec = importlib.util.spec_from_file_location("nocturne_supply_tool_test", TOOL_PATH)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_fetch_destination_is_confined_and_marker_owned(tmp_path, monkeypatch):
    tool = load_tool()
    output = tmp_path / "output"
    output.mkdir()
    monkeypatch.setattr(tool, "OUTPUT", output)

    with pytest.raises(SystemExit):
        tool._prepare_fetch_destination(output)
    with pytest.raises(SystemExit):
        tool._prepare_fetch_destination(tmp_path / "outside")

    unmarked = output / "unmarked"
    unmarked.mkdir()
    sentinel = unmarked / "sentinel"
    sentinel.write_text("preserve", encoding="ascii")
    with pytest.raises(SystemExit):
        tool._prepare_fetch_destination(unmarked)
    assert sentinel.read_text(encoding="ascii") == "preserve"

    external = tmp_path / "external"
    external.mkdir()
    linked = output / "linked"
    linked.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        tool._prepare_fetch_destination(linked)
    assert linked.is_symlink()

    owned = tool._prepare_fetch_destination(output / "owned")
    marker = owned / tool.OUTPUT_MARKER
    assert marker.read_text(encoding="ascii") == tool.OUTPUT_MARKER_CONTENT
    generated = owned / "generated"
    generated.write_text("replaceable", encoding="ascii")
    assert tool._prepare_fetch_destination(owned) == owned
    assert not generated.exists() and marker.is_file()
    assert stat.S_ISREG(marker.stat().st_mode)


def test_fetch_cli_rejects_external_absolute_path_before_network(tmp_path):
    external = tmp_path / "external"
    external.mkdir()
    sentinel = external / "sentinel"
    sentinel.write_text("preserve", encoding="ascii")
    result = subprocess.run(
        [sys.executable, str(TOOL_PATH), "fetch", str(external)],
        cwd=REPO,
        text=True,
        capture_output=True,
    )
    assert result.returncode != 0
    assert "must be a child directory" in result.stderr
    assert sentinel.read_text(encoding="ascii") == "preserve"


def test_dockerfile_normalizes_generated_account_dates():
    dockerfile = (REPO / "deploy/nocturne/Dockerfile").read_text(encoding="utf-8")
    assert "shadow_day=$((SOURCE_DATE_EPOCH / 86400))" in dockerfile
    assert 'sed -i "s/^\\(appuser:[^:]*:\\)[^:]*/\\1${shadow_day}/" /etc/shadow' in dockerfile
    assert 'grep -q "^appuser:[^:]*:${shadow_day}:" /etc/shadow' in dockerfile


def test_input_lock_rejects_unused_or_tampered_claims(tmp_path, monkeypatch):
    tool = load_tool()
    source_root = REPO / "deploy/nocturne"
    for name in (
        "supply-chain.lock.json",
        "image.lock.json",
        "requirements.lock",
        "build-requirements.lock",
        "Dockerfile",
        "overlay/edu_agent_maintenance.py",
        "overlay/web_app.py",
        "backup-inventory.schema.json",
        "scripts/tool.py",
    ):
        target = tmp_path / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source_root / name, target)
    lock_path = tmp_path / "supply-chain.lock.json"
    lock = json.loads(lock_path.read_text(encoding="utf-8"))
    lock["build"]["unused_buildkit_tarball_sha256"] = "0" * 64
    lock_path.write_text(json.dumps(lock), encoding="utf-8")
    monkeypatch.setattr(tool, "ROOT", tmp_path)
    monkeypatch.setattr(tool, "LOCK_PATH", lock_path)
    monkeypatch.setattr(tool, "IMAGE_LOCK_PATH", tmp_path / "image.lock.json")
    with pytest.raises(SystemExit, match="build lock shape is invalid"):
        tool.verify_lock()


def test_buildx_artifact_hash_tamper_fails_closed(tmp_path):
    tool = load_tool()
    source = tmp_path / "docker-buildx"
    source.write_bytes(b"locked buildx fixture")
    expected = hashlib.sha256(source.read_bytes()).hexdigest()
    downloaded = tmp_path / "good" / "docker-buildx"
    assert tool.download_verified(source.as_uri(), downloaded, expected, "Buildx artifact") == downloaded
    assert downloaded.read_bytes() == source.read_bytes()

    rejected = tmp_path / "bad" / "docker-buildx"
    with pytest.raises(SystemExit, match="Buildx artifact SHA-256 mismatch"):
        tool.download_verified(source.as_uri(), rejected, "0" * 64, "Buildx artifact")
    assert not rejected.exists()
