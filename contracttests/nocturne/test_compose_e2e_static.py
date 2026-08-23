from __future__ import annotations

import json
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "contracttests/nocturne/run-compose-e2e.sh"
DRIVER = ROOT / "contracttests/nocturne/compose_e2e.py"
SUPPLY_TOOL = ROOT / "deploy/nocturne/scripts/tool.py"
COMPOSE = ROOT / "deploy/compose.yaml"
ENV_EXAMPLE = ROOT / "deploy/env.example"
FAILED_FORWARD = ROOT / "contracttests/nocturne/failed_forward.py"


def test_nocturne_snapshot_backup_and_database_volumes_are_independent_and_internal_only():
    completed = subprocess.run(
        ["docker", "compose", "-f", str(COMPOSE), "--env-file", str(ENV_EXAMPLE), "config", "--format", "json"],
        check=True, capture_output=True, text=True,
    )
    config = json.loads(completed.stdout)
    nocturne = config["services"]["nocturne"]
    mounts = {item["target"]: item["source"] for item in nocturne["volumes"]}
    assert mounts["/app/snapshots"] == "nocturne-snapshots"
    assert mounts["/app/backups"] == "nocturne-backups"
    database_mount = config["services"]["nocturne-postgres"]["volumes"][0]
    assert database_mount["target"] == "/var/lib/postgresql/data"
    assert len({mounts["/app/snapshots"], mounts["/app/backups"], database_mount["source"]}) == 3
    assert nocturne["environment"]["SNAPSHOT_DIR"] == "/app/snapshots"
    assert set(nocturne["networks"]) == {"nocturne-internal"}
    assert config["networks"]["nocturne-internal"]["internal"] is True
    assert "ports" not in nocturne


def test_compose_gate_has_secret_safe_cleanup_and_verified_oci_entrypoint():
    shell = SCRIPT.read_text(encoding="utf-8")
    assert "set -eu" in shell and "umask 077" in shell
    assert "trap cleanup EXIT HUP INT TERM" in shell
    assert "down --volumes --remove-orphans" in shell
    assert "edu-agent.nocturne.rollback.project=$PROJECT" in shell
    assert "com.docker.compose.project=$PROJECT" in shell
    assert "tool.py\" verify-oci" in shell and "skopeo copy --preserve-digests" in shell
    assert "failed_forward.Dockerfile" in shell and "FAILED_FORWARD_IMAGE_REF" in shell and "docker push" in shell
    assert "mktemp -d" in shell and "rm -rf \"$TMP_DIR\"" in shell
    assert "set -x" not in shell and "eval " not in shell and "source " not in shell
    assert "echo $" not in shell and "printenv" not in shell and "docker compose config" not in shell
    subprocess.run(["sh", "-n", str(SCRIPT)], check=True)


def test_compose_gate_covers_real_runtime_phases_without_printing_secrets():
    driver = DRIVER.read_text(encoding="utf-8")
    required = [
        "check_auth_and_allowlist",
        "check_real_nocturne_crud_and_absence",
        "check_real_delivery_expiry_reconciliation",
        "check_database_account_isolation",
        "check_down_queue_and_replay",
        "check_real_rollback_rehearsal",
        "apply_failed_forward_upgrade",
        "restore_original_nocturne_database",
        "check_backup_encryption_key_destruction_and_prune",
        "check_destroyed_key_rollback_failure",
        "check_sigterm_shutdown",
        'for service in ("postgres", "nocturne-postgres", "nocturne", "server")',
        'body.get("status") == expected',
        'public_status") != "queued"',
        'public_status") == "applied"',
        "to_regclass('public.nodes')",
        "to_regclass('public.memory_candidates')",
        "memory_expiry_reconciliations",
        "zz-e2e-secondary",
        "rollback.sh",
        "repository + \":floating\"",
        "failed-isolated",
        "NOCTURNE_FAILED_FORWARD_IMAGE",
        "nodes_pre_a84_failed_forward",
        "failed_forward_image",
        "restore-original",
        "PGDMP",
        "EDUMBKUP",
    ]
    for marker in required:
        assert marker in driver
    assert "print(self.env" not in driver and "print(self.token" not in driver and "print(grant" not in driver
    subprocess.run(["python3", "-m", "py_compile", str(DRIVER), str(FAILED_FORWARD)], check=True)


def test_locked_oci_build_is_explicitly_no_cache():
    source = SUPPLY_TOOL.read_text(encoding="utf-8")
    assert '"buildx", "build"' in source
    assert '"--no-cache"' in source
