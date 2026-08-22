from __future__ import annotations

import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "contracttests/nocturne/run-compose-e2e.sh"
DRIVER = ROOT / "contracttests/nocturne/compose_e2e.py"


def test_compose_gate_has_secret_safe_cleanup_and_verified_oci_entrypoint():
    shell = SCRIPT.read_text(encoding="utf-8")
    assert "set -eu" in shell and "umask 077" in shell
    assert "trap cleanup EXIT HUP INT TERM" in shell
    assert "down --volumes --remove-orphans" in shell
    assert "tool.py\" verify-oci" in shell and "skopeo copy --preserve-digests" in shell
    assert "mktemp -d" in shell and "rm -rf \"$TMP_DIR\"" in shell
    assert "set -x" not in shell and "eval " not in shell and "source " not in shell
    assert "echo $" not in shell and "printenv" not in shell and "docker compose config" not in shell
    subprocess.run(["sh", "-n", str(SCRIPT)], check=True)


def test_compose_gate_covers_real_runtime_phases_without_printing_secrets():
    driver = DRIVER.read_text(encoding="utf-8")
    required = [
        "check_auth_and_allowlist",
        "check_real_nocturne_crud_and_absence",
        "check_database_account_isolation",
        "check_down_queue_and_replay",
        "check_backup_encryption_key_destruction_and_prune",
        "check_sigterm_shutdown",
        'for service in ("postgres", "nocturne-postgres", "nocturne", "server")',
        'body.get("status") == expected',
        'public_status") != "queued"',
        'public_status") == "applied"',
        "to_regclass('public.nodes')",
        "to_regclass('public.memory_candidates')",
        "EDUMBKUP",
    ]
    for marker in required:
        assert marker in driver
    assert "print(self.env" not in driver and "print(self.token" not in driver and "print(grant" not in driver
    subprocess.run(["python3", "-m", "py_compile", str(DRIVER)], check=True)
