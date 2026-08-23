#!/usr/bin/env python3
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path
from urllib.parse import quote


POSTGRES_IMAGE = "postgres:15-alpine@sha256:fe0737ba566a2c5b2a28f34433c0a423261900ec17b9bf7ad115e1aae7e57f1b"
IMAGE_RE = re.compile(r"^(?P<repository>[a-z0-9][a-z0-9._:-]*(?:/[a-z0-9][a-z0-9._-]*)*)@sha256:(?P<digest>[0-9a-f]{64})$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
VOLUME_RE = re.compile(r"^[a-z0-9][a-z0-9_.-]{0,127}$")
ARTIFACT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$")
SCHEMA_RE = re.compile(r"^[0-9][A-Za-z0-9_.-]{0,254}\.py$")
HEX40_RE = re.compile(r"^[0-9a-f]{40}$")
HEX64_RE = re.compile(r"^[0-9a-f]{64}$")
FIXED_NAME_RE = re.compile(r"^[a-z][a-z0-9_-]{0,62}$")
PROJECT_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,62}$")


class RollbackError(RuntimeError):
    pass


def validate_image_reference(value: str) -> tuple[str, str]:
    match = IMAGE_RE.fullmatch(value)
    if match is None or ":" in match.group("repository").rsplit("/", 1)[-1]:
        raise RollbackError("old Nocturne image must be a canonical repository@sha256 reference")
    return match.group("repository"), "sha256:" + match.group("digest")


def validate_target_volume(target: str, original: str) -> None:
    if VOLUME_RE.fullmatch(target) is None:
        raise RollbackError("rollback volume name is invalid")
    if target == original:
        raise RollbackError("rollback target must not reuse the original Nocturne database volume")


def assert_target_database_empty(value: str) -> None:
    try:
        count = int(value.strip())
    except ValueError as exc:
        raise RollbackError("rollback target database emptiness could not be verified") from exc
    if count != 0:
        raise RollbackError("rollback target database is not empty")


def validate_artifact_path(value: str) -> None:
    if ARTIFACT_RE.fullmatch(value) is None or value in {"managed-inventory.json", ".edu-agent-backup.lock", ".", ".."}:
        raise RollbackError("backup artifact must be a flat relative managed path")


def read_env_file(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        raise RollbackError("rollback environment file is unavailable") from exc
    for line in lines:
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            raise RollbackError("rollback environment file is invalid")
        key, value = line.split("=", 1)
        if not key or key.strip() != key or "\x00" in value:
            raise RollbackError("rollback environment file is invalid")
        values[key] = value
    return values


def json_string(value: str) -> str:
    return json.dumps(value, ensure_ascii=True)


class Runner:
    def run(
        self,
        command: list[str],
        label: str,
        *,
        allowed: tuple[int, ...] = (0,),
        input_text: str | None = None,
        env: dict[str, str] | None = None,
    ) -> subprocess.CompletedProcess[str]:
        completed = subprocess.run(command, input=input_text, text=True, capture_output=True, env=env)
        if completed.returncode not in allowed:
            raise RollbackError(label)
        return completed


class Rollback:
    def __init__(self, args: argparse.Namespace, runner: Runner | None = None):
        self.args = args
        self.runner = runner or Runner()
        self.compose_file = Path(args.compose_file).resolve()
        self.env_file = Path(args.env_file).resolve()
        self.record_path = Path(args.record).resolve()
        self.override_path = self.record_path.with_name(self.record_path.stem + ".compose.yaml")
        self.values = read_env_file(self.env_file)
        self.environment = os.environ.copy()
        self.environment["NOCTURNE_ROLLBACK_IMAGE"] = args.old_image
        self.original_volume = ""
        self.target_created = False
        self.snapshot_created = False
        self.writer_stopped = False
        self.record: dict[str, object] = {}
        self._validate_static_inputs()

    @property
    def compose_base(self) -> list[str]:
        result = ["docker", "compose", "-f", str(self.compose_file)]
        if self.override_path.exists():
            result += ["-f", str(self.override_path)]
        return result + ["--env-file", str(self.env_file), "-p", self.args.project]

    def compose(self, *arguments: str, label: str, input_text: str | None = None) -> subprocess.CompletedProcess[str]:
        return self.runner.run(self.compose_base + list(arguments), label, input_text=input_text, env=self.environment)

    def _validate_static_inputs(self) -> None:
        if shutil.which("docker") is None:
            raise RollbackError("docker is unavailable")
        self.runner.run(["docker", "compose", "version"], "Docker Compose is unavailable")
        if not self.compose_file.is_file():
            raise RollbackError("Compose file is unavailable")
        if PROJECT_RE.fullmatch(self.args.project) is None:
            raise RollbackError("Compose project name is invalid")
        validate_image_reference(self.args.old_image)
        if self.args.expected_platform not in {"linux/amd64", "linux/arm64"}:
            raise RollbackError("expected image platform is invalid")
        if DIGEST_RE.fullmatch(self.args.expected_config_digest) is None:
            raise RollbackError("expected image config digest is invalid")
        if HEX40_RE.fullmatch(self.args.expected_upstream_commit) is None:
            raise RollbackError("expected upstream commit is invalid")
        if not self.args.expected_compat_revision or len(self.args.expected_compat_revision) > 128:
            raise RollbackError("expected compatibility revision is invalid")
        if SCHEMA_RE.fullmatch(self.args.expected_schema_version) is None:
            raise RollbackError("expected schema migration version is invalid")
        validate_artifact_path(self.args.artifact)
        if not self.args.seed_path or self.args.seed_path.startswith("/") or ".." in self.args.seed_path.split("/"):
            raise RollbackError("seed path is invalid")
        if not self.args.seed_search_query.strip() or len(self.args.seed_search_query) > 200:
            raise RollbackError("seed search query is invalid")
        if HEX64_RE.fullmatch(self.args.seed_content_sha256) is None:
            raise RollbackError("seed content digest is invalid")
        if not self.record_path.is_absolute() or self.record_path.parent.resolve() != self.record_path.parent:
            raise RollbackError("rollback record path must be canonical and absolute")
        if not self.record_path.parent.is_dir() or self.record_path.exists() or self.override_path.exists():
            raise RollbackError("rollback record and Compose override must be new files")
        if self.args.target_volume == self.args.target_snapshot_volume:
            raise RollbackError("rollback database and snapshot volumes must differ")
        required = [
            "NOCTURNE_POSTGRES_DB", "NOCTURNE_POSTGRES_USER", "NOCTURNE_POSTGRES_PASSWORD",
            "NOCTURNE_BRIDGE_API_TOKEN", "NOCTURNE_MAINTENANCE_TOKEN",
        ]
        if any(not self.values.get(name) for name in required):
            raise RollbackError("rollback environment is missing required Nocturne values")
        if FIXED_NAME_RE.fullmatch(self.values["NOCTURNE_POSTGRES_DB"]) is None or FIXED_NAME_RE.fullmatch(self.values["NOCTURNE_POSTGRES_USER"]) is None:
            raise RollbackError("Nocturne database name or user is invalid")

    def _write_record(self, status: str, reason: str = "") -> None:
        self.record["status"] = status
        self.record["updated_at"] = dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
        if reason:
            self.record["failure_reason"] = reason
        else:
            self.record.pop("failure_reason", None)
        temporary = self.record_path.with_name(self.record_path.name + ".tmp")
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                json.dump(self.record, stream, sort_keys=True, indent=2)
                stream.write("\n")
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.record_path)
        finally:
            try:
                temporary.unlink()
            except FileNotFoundError:
                pass

    def _discover_original_volume(self) -> None:
        container = self.compose("ps", "-q", "nocturne-postgres", label="original Nocturne database container is unavailable").stdout.strip()
        if not container:
            raise RollbackError("original Nocturne database container is unavailable")
        inspected = self.runner.run(["docker", "inspect", container], "original Nocturne database mount could not be inspected")
        try:
            mounts = json.loads(inspected.stdout)[0]["Mounts"]
            mount = next(item for item in mounts if item.get("Destination") == "/var/lib/postgresql/data")
            if mount.get("Type") != "volume" or not mount.get("Name"):
                raise KeyError
        except (IndexError, KeyError, StopIteration, TypeError, json.JSONDecodeError) as exc:
            raise RollbackError("original Nocturne database volume could not be identified") from exc
        self.original_volume = mount["Name"]
        validate_target_volume(self.args.target_volume, self.original_volume)
        validate_target_volume(self.args.target_snapshot_volume, self.original_volume)

    def _assert_new_volume(self, name: str) -> None:
        result = self.runner.run(["docker", "volume", "inspect", name], "rollback volume inspection failed", allowed=(0, 1))
        if result.returncode == 0:
            raise RollbackError("rollback target volume already exists")

    def _inspect_old_image(self) -> None:
        inspected = self.runner.run(
            ["docker", "image", "inspect", self.args.old_image],
            "old Nocturne image inspection failed",
            allowed=(0, 1),
        )
        if inspected.returncode != 0:
            for attempt in range(3):
                pulled = subprocess.run(
                    ["docker", "pull", self.args.old_image],
                    text=True,
                    capture_output=True,
                    env=self.environment,
                )
                if pulled.returncode == 0:
                    break
                if attempt == 2:
                    raise RollbackError("old Nocturne image pull failed")
                time.sleep(attempt + 1)
            inspected = self.runner.run(
                ["docker", "image", "inspect", self.args.old_image],
                "old Nocturne image inspection failed",
            )
        manifest = None
        for attempt in range(3):
            current = subprocess.run(
                ["docker", "buildx", "imagetools", "inspect", "--raw", self.args.old_image],
                text=True,
                capture_output=True,
                env=self.environment,
            )
            if current.returncode == 0:
                manifest = current
                break
            if attempt < 2:
                time.sleep(attempt + 1)
        if manifest is None:
            raise RollbackError("old Nocturne image manifest inspection failed")
        try:
            image = json.loads(inspected.stdout)[0]
            manifest_payload = json.loads(manifest.stdout)
            platform = f"{image['Os']}/{image['Architecture']}"
            config_digest = manifest_payload["config"]["digest"]
        except (IndexError, KeyError, TypeError, json.JSONDecodeError) as exc:
            raise RollbackError("old Nocturne image metadata is invalid") from exc
        if platform != self.args.expected_platform:
            raise RollbackError("old Nocturne image platform does not match the recorded value")
        if config_digest != self.args.expected_config_digest:
            raise RollbackError("old Nocturne image config digest does not match the recorded value")
        _, manifest_digest = validate_image_reference(self.args.old_image)
        self.record = {
            "schema": "edu-agent.nocturne-rollback.v1",
            "project": self.args.project,
            "artifact": self.args.artifact,
            "old_image": self.args.old_image,
            "platform": platform,
            "platform_manifest_digest": manifest_digest,
            "config_digest": config_digest,
            "expected_upstream_commit": self.args.expected_upstream_commit,
            "expected_compat_revision": self.args.expected_compat_revision,
            "expected_schema_version": self.args.expected_schema_version,
            "original_database_volume": self.original_volume,
            "target_database_volume": self.args.target_volume,
            "target_snapshot_volume": self.args.target_snapshot_volume,
            "compose_override": str(self.override_path),
            "bridge_writer": "not-stopped",
        }
        self._write_record("image-verified")

    def _stop_writers(self) -> None:
        self.compose("stop", "server", label="bridge writer could not be stopped")
        self.writer_stopped = True
        self.record["bridge_writer"] = "stopped"
        self._write_record("writer-stopped")
        self.compose("stop", "nocturne", label="current Nocturne service could not be stopped")

    def _preflight_restore(self) -> None:
        script = (
            'artifact=$1; output=/dev/shm/edu-agent-rollback.dump; code=0; '
            'if ! edu-agentd nocturne-backup restore --artifact "$artifact" --output "$output"; then code=41; '
            'elif [ "$(stat -c %a "$output")" != 600 ]; then code=42; '
            'elif ! pg_restore --list "$output" >/dev/null; then code=43; fi; '
            'rm -f "$output"; printf "%s\\n" "$code"'
        )
        command = self.compose_base + [
            "run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "server",
            "-eu", "-c", script, "rollback-verify", self.args.artifact,
        ]
        completed = subprocess.run(command, text=True, capture_output=True, env=self.environment)
        if completed.returncode != 0:
            detail = completed.stderr.strip().splitlines()[-1] if completed.stderr.strip() else "docker compose run failed"
            for value in self.values.values():
                if value:
                    detail = detail.replace(value, "[REDACTED]")
            raise RollbackError("managed backup restore preflight execution failed: " + detail[:240])
        try:
            stage = int(completed.stdout.strip().splitlines()[-1])
        except (IndexError, ValueError) as exc:
            raise RollbackError("managed backup restore preflight result is invalid") from exc
        if stage == 41:
            raise RollbackError("managed backup artifact or generation key is not recoverable")
        if stage == 42:
            raise RollbackError("managed backup restore output permissions are invalid")
        if stage == 43:
            raise RollbackError("managed backup plaintext is not a PostgreSQL custom dump")
        if stage != 0:
            raise RollbackError("managed backup restore preflight result is invalid")

    def _create_volumes(self) -> None:
        labels = ["--label", "edu-agent.nocturne.rollback=true", "--label", f"edu-agent.nocturne.rollback.project={self.args.project}"]
        self.runner.run(["docker", "volume", "create", *labels, self.args.target_volume], "rollback database volume creation failed")
        self.target_created = True
        self.runner.run(["docker", "volume", "create", *labels, self.args.target_snapshot_volume], "rollback snapshot volume creation failed")
        self.snapshot_created = True

    def _write_override(self) -> None:
        user = quote(self.values["NOCTURNE_POSTGRES_USER"], safe="")
        password = quote(self.values["NOCTURNE_POSTGRES_PASSWORD"], safe="")
        database = quote(self.values["NOCTURNE_POSTGRES_DB"], safe="")
        async_dsn = f"postgresql+asyncpg://{user}:{password}@nocturne-rollback-postgres:5432/{database}?ssl=disable"
        restore_dsn = f"postgres://{user}:{password}@nocturne-rollback-postgres:5432/{database}?sslmode=disable"
        content = f"""services:
  nocturne-rollback-postgres:
    image: {POSTGRES_IMAGE}
    environment:
      POSTGRES_DB: ${{NOCTURNE_POSTGRES_DB:-nocturne}}
      POSTGRES_USER: ${{NOCTURNE_POSTGRES_USER:-nocturne}}
      POSTGRES_PASSWORD: ${{NOCTURNE_POSTGRES_PASSWORD:?set NOCTURNE_POSTGRES_PASSWORD}}
      TZ: UTC
    volumes:
      - nocturne-rollback-database:/var/lib/postgresql/data
    healthcheck:
      test: [\"CMD-SHELL\", \"pg_isready -U $${{POSTGRES_USER}} -d $${{POSTGRES_DB}}\"]
      interval: 2s
      timeout: 3s
      retries: 30
    networks:
      - nocturne-internal
  nocturne:
    image: ${{NOCTURNE_ROLLBACK_IMAGE:?set NOCTURNE_ROLLBACK_IMAGE}}
    pull_policy: never
    environment:
      DATABASE_URL: {json_string(async_dsn)}
      SNAPSHOT_DIR: /app/snapshots
    volumes:
      - nocturne-backups:/app/backups:ro
      - nocturne-rollback-snapshots:/app/snapshots
  server:
    environment:
      NOCTURNE_ROLLBACK_DATABASE_URL: {json_string(restore_dsn)}
volumes:
  nocturne-rollback-database:
    external: true
    name: {json_string(self.args.target_volume)}
  nocturne-rollback-snapshots:
    external: true
    name: {json_string(self.args.target_snapshot_volume)}
"""
        descriptor = os.open(self.override_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())

    def _wait_healthy(self, service: str, timeout: float = 120.0) -> str:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            container = self.compose("ps", "-q", service, label="rollback service status is unavailable").stdout.strip()
            if container:
                status = self.runner.run(
                    ["docker", "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", container],
                    "rollback service health inspection failed",
                ).stdout.strip()
                if status == "healthy":
                    return container
            time.sleep(1)
        raise RollbackError("rollback service did not become healthy")

    def _start_empty_target(self) -> None:
        self.compose("stop", "nocturne-postgres", label="original Nocturne database could not be isolated")
        self.compose("up", "-d", "nocturne-rollback-postgres", label="rollback target database could not be started")
        container = self._wait_healthy("nocturne-rollback-postgres")
        inspected = self.runner.run(["docker", "inspect", container], "rollback target database mount could not be inspected")
        try:
            mounts = json.loads(inspected.stdout)[0]["Mounts"]
            source = next(item["Name"] for item in mounts if item.get("Destination") == "/var/lib/postgresql/data")
        except (IndexError, KeyError, StopIteration, TypeError, json.JSONDecodeError) as exc:
            raise RollbackError("rollback target database mount is invalid") from exc
        if source != self.args.target_volume or source == self.original_volume:
            raise RollbackError("rollback target database is not mounted from the new isolated volume")
        query = "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','S','f')"
        count = self.compose(
            "exec", "-T", "nocturne-rollback-postgres", "sh", "-eu", "-c",
            'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "$1"', "rollback-empty-check", query,
            label="rollback target database emptiness check failed",
        ).stdout
        assert_target_database_empty(count)

    def _import_backup(self) -> None:
        script = (
            'artifact=$1; output=/dev/shm/edu-agent-rollback.dump; '
            'edu-agentd nocturne-backup restore --artifact "$artifact" --output "$output"; '
            'pg_restore --exit-on-error --no-owner --no-privileges --dbname="$NOCTURNE_ROLLBACK_DATABASE_URL" "$output"; '
            'rm -f "$output"'
        )
        self.compose(
            "run", "--rm", "--no-deps", "--entrypoint", "/bin/sh", "server",
            "-eu", "-c", script, "rollback-import", self.args.artifact,
            label="managed backup could not be imported into the isolated rollback database",
        )

    def _verify_schema(self) -> None:
        query = "SELECT COALESCE(max(version),'') FROM schema_migrations"
        version = self.compose(
            "exec", "-T", "nocturne-rollback-postgres", "sh", "-eu", "-c",
            'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "$1"', "rollback-schema-check", query,
            label="rollback schema migration version could not be read",
        ).stdout.strip()
        if version != self.args.expected_schema_version:
            raise RollbackError("restored schema migration version does not match the old locked image")

    def _start_and_probe_old_image(self) -> None:
        self.compose("up", "-d", "--no-deps", "nocturne", label="old locked Nocturne image could not be started")
        container = self._wait_healthy("nocturne")
        running_manifest = self.runner.run(["docker", "inspect", "--format", "{{.Image}}", container], "running old image digest could not be verified").stdout.strip()
        _, expected_manifest = validate_image_reference(self.args.old_image)
        if running_manifest != expected_manifest:
            raise RollbackError("running Nocturne container does not use the recorded old platform manifest")
        self._verify_schema()
        probe_path = Path(__file__).with_name("rollback_probe.py")
        try:
            probe_program = probe_path.read_text(encoding="utf-8")
        except OSError as exc:
            raise RollbackError("rollback validation probe is unavailable") from exc
        request = json.dumps({
            "expected_upstream_commit": self.args.expected_upstream_commit,
            "expected_compat_revision": self.args.expected_compat_revision,
            "seed_path": self.args.seed_path,
            "seed_search_query": self.args.seed_search_query,
            "seed_content_sha256": self.args.seed_content_sha256,
        }, sort_keys=True)
        self.compose(
            "exec", "-T", "nocturne", "python", "-c", probe_program,
            label="old locked Nocturne image failed health or restored-data validation", input_text=request,
        )

    def execute(self) -> None:
        try:
            self._discover_original_volume()
            self._assert_new_volume(self.args.target_volume)
            self._assert_new_volume(self.args.target_snapshot_volume)
            self._inspect_old_image()
            self._stop_writers()
            self._preflight_restore()
            self._create_volumes()
            self._write_override()
            self._start_empty_target()
            self._import_backup()
            self._start_and_probe_old_image()
            self.record["bridge_writer"] = "stopped-pending-operator-release"
            self._write_record("validated")
        except RollbackError as exc:
            if self.writer_stopped:
                try:
                    if self.override_path.exists():
                        self.compose("stop", "nocturne", "nocturne-rollback-postgres", label="rollback isolation stop failed")
                    else:
                        self.compose("stop", "nocturne", label="rollback isolation stop failed")
                except RollbackError:
                    pass
            if self.record:
                try:
                    self._write_record("failed-isolated", str(exc))
                except (OSError, RollbackError):
                    pass
            raise


def parse_args() -> argparse.Namespace:
    root = Path(__file__).resolve().parents[3]
    parser = argparse.ArgumentParser(description="Restore an encrypted pre-upgrade Nocturne backup into a new isolated database")
    parser.add_argument("--compose-file", default=str(root / "deploy/compose.yaml"))
    parser.add_argument("--env-file", required=True)
    parser.add_argument("--project", required=True)
    parser.add_argument("--artifact", required=True)
    parser.add_argument("--old-image", required=True)
    parser.add_argument("--expected-platform", required=True)
    parser.add_argument("--expected-config-digest", required=True)
    parser.add_argument("--expected-upstream-commit", required=True)
    parser.add_argument("--expected-compat-revision", required=True)
    parser.add_argument("--expected-schema-version", required=True)
    parser.add_argument("--target-volume", required=True)
    parser.add_argument("--target-snapshot-volume", required=True)
    parser.add_argument("--seed-path", required=True)
    parser.add_argument("--seed-search-query", required=True)
    parser.add_argument("--seed-content-sha256", required=True)
    parser.add_argument("--record", required=True)
    return parser.parse_args()


def main() -> int:
    try:
        Rollback(parse_args()).execute()
    except RollbackError as exc:
        print(f"rollback failed: {exc}", file=sys.stderr)
        return 1
    print("rollback target validated; bridge writer remains stopped pending operator release", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
