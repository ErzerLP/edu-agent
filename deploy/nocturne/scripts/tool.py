#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parents[1]
LOCK_PATH = ROOT / "supply-chain.lock.json"
IMAGE_LOCK_PATH = ROOT / "image.lock.json"
OUTPUT = ROOT / "output"
OUTPUT_MARKER = ".edu-agent-nocturne-output-v1"
OUTPUT_MARKER_CONTENT = "owned by deploy/nocturne/scripts/tool.py\n"
_HEX = set("0123456789abcdef")


def require_keys(value: object, keys: set[str], label: str) -> dict:
    if not isinstance(value, dict) or set(value) != keys:
        raise SystemExit(f"{label} lock shape is invalid")
    return value


def require_digest(value: object, label: str, *, prefixed: bool = False) -> str:
    if not isinstance(value, str):
        raise SystemExit(f"{label} must be a SHA-256 digest")
    raw = value.removeprefix("sha256:")
    if (prefixed and not value.startswith("sha256:")) or len(raw) != 64 or set(raw) - _HEX:
        raise SystemExit(f"{label} must be a SHA-256 digest")
    return raw


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def load_lock() -> dict:
    try:
        lock = json.loads(LOCK_PATH.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise SystemExit("input lock is not valid JSON") from exc
    return require_keys(lock, {"schema", "upstream", "overlay", "build", "python", "debian", "image_contract"}, "input")


def verify_lock() -> dict:
    lock = load_lock()
    if lock["schema"] != "edu-agent.nocturne-supply-chain-lock.v1":
        raise SystemExit("input lock schema mismatch")
    upstream = require_keys(lock["upstream"], {"repository", "tag", "tag_object", "commit", "source_archive_url", "source_archive_sha256"}, "upstream")
    overlay = require_keys(lock["overlay"], {"revision", "files"}, "overlay")
    overlay_files = require_keys(
        overlay["files"],
        {"Dockerfile", "overlay/edu_agent_maintenance.py", "overlay/web_app.py", "backup-inventory.schema.json", "scripts/tool.py"},
        "overlay files",
    )
    for relative, expected in overlay_files.items():
        require_digest(expected, f"overlay file {relative}")
        if sha256(ROOT / relative) != expected:
            raise SystemExit(f"overlay file digest mismatch: {relative}")
    if overlay["revision"] != "edu-agent-maintenance-v1":
        raise SystemExit("overlay revision mismatch")
    build = require_keys(lock["build"], {"buildkit_version", "buildkit_image", "buildx_version", "buildx_linux_amd64_url", "buildx_linux_amd64_sha256", "platform", "source_date_epoch", "base_image"}, "build")
    base = require_keys(build["base_image"], {"reference", "platform_manifest_digest"}, "base image")
    python_lock = require_keys(lock["python"], {"version", "requirements_file", "requirements_sha256", "resolved_distributions", "artifact_hash_policy", "build_requirements_file", "build_requirements_sha256"}, "Python")
    debian = require_keys(lock["debian"], {"snapshot", "suite", "repository", "signed_release_required", "explicit_install"}, "Debian")
    image_contract = require_keys(lock["image_contract"], {"entrypoint", "labels"}, "image contract")

    require_digest(upstream["source_archive_sha256"], "source archive")
    require_digest(base["platform_manifest_digest"], "base platform manifest", prefixed=True)
    require_digest(build["buildx_linux_amd64_sha256"], "Buildx artifact")
    if not isinstance(build["buildkit_image"], str) or "@sha256:" not in build["buildkit_image"]:
        raise SystemExit("BuildKit image must be digest pinned")
    require_digest(build["buildkit_image"].split("@", 1)[1], "BuildKit image", prefixed=True)
    if build["platform"] != "linux/amd64" or not isinstance(build["source_date_epoch"], int):
        raise SystemExit("build platform or epoch is invalid")
    if not isinstance(build["buildx_version"], str) or build["buildx_version"] not in build["buildx_linux_amd64_url"]:
        raise SystemExit("Buildx artifact URL is not version pinned")
    if not isinstance(build["buildkit_version"], str) or not build["buildkit_version"].startswith("v"):
        raise SystemExit("BuildKit version is invalid")
    if (
        upstream["tag"] != "2.5.6"
        or upstream["tag_object"] != "341131c34f42fc9b8401fc4ef6dc01b4fe9f11d4"
        or upstream["commit"] != "54c48eeaeea3cca61ff6bc065cbe1a4c32a3b254"
    ):
        raise SystemExit("upstream tag provenance mismatch")
    if upstream["commit"] not in upstream["source_archive_url"]:
        raise SystemExit("source archive URL is not commit pinned")
    if python_lock["version"] != "3.12.11" or python_lock["version"] not in base["reference"]:
        raise SystemExit("Python runtime version is not bound to the base image")
    if python_lock["artifact_hash_policy"] != "all hashes published for every resolved distribution are retained; installation requires hashes":
        raise SystemExit("Python artifact hash policy mismatch")
    if debian["snapshot"] != "20260801T000000Z" or debian["snapshot"] not in debian["repository"]:
        raise SystemExit("Debian snapshot is not bound to the repository URL")

    requirements = ROOT / python_lock["requirements_file"]
    build_requirements = ROOT / python_lock["build_requirements_file"]
    require_digest(python_lock["requirements_sha256"], "requirements lock")
    require_digest(python_lock["build_requirements_sha256"], "build requirements lock")
    if sha256(build_requirements) != python_lock["build_requirements_sha256"]:
        raise SystemExit("build requirements lock digest mismatch")
    if sha256(requirements) != python_lock["requirements_sha256"]:
        raise SystemExit("requirements lock digest mismatch")
    text = requirements.read_text(encoding="utf-8")
    distributions = [line for line in text.splitlines() if line and not line.startswith((" ", "#", "-")) and "==" in line]
    if len(distributions) != python_lock["resolved_distributions"] or "--hash=sha256:" not in text:
        raise SystemExit("requirements artifact lock is incomplete")

    dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
    expected_from = f"FROM {base['reference']}@{base['platform_manifest_digest']}"
    if expected_from not in dockerfile or 'CMD ["python", "main.py"]' not in dockerfile or "run_sse.py" in dockerfile:
        raise SystemExit("Dockerfile does not match the image contract")
    labels = require_keys(image_contract["labels"], {"org.opencontainers.image.source", "org.opencontainers.image.version", "org.opencontainers.image.revision", "org.edu-agent.nocturne.compat-revision"}, "image labels")
    for value in labels.values():
        if not isinstance(value, str) or value not in dockerfile:
            raise SystemExit("Dockerfile label mismatch")

    if not debian["signed_release_required"]:
        raise SystemExit("Debian snapshot must require signed Release metadata")
    apt_source = f"deb [check-valid-until=no] {debian['repository']} {debian['suite']} main"
    if apt_source not in dockerfile or "trusted=yes" in dockerfile or "allow-unauthenticated" in dockerfile.lower():
        raise SystemExit("Dockerfile does not enforce the signed Debian snapshot")
    if not isinstance(debian["explicit_install"], list) or not debian["explicit_install"] or any(not isinstance(item, str) or not re.fullmatch(r"[a-z0-9][a-z0-9+.-]*=[^=\\\s]+", item) for item in debian["explicit_install"]):
        raise SystemExit("Debian package versions are not exact")
    install_match = re.search(r"apt-get install -y --no-install-recommends(?P<body>.*?)&&", dockerfile, re.DOTALL)
    installed = install_match.group("body").replace("\\", " ").split() if install_match else []
    if installed != debian["explicit_install"]:
        raise SystemExit("Dockerfile package set does not match the Debian input lock")

    if "placeholder" in json.dumps(lock).lower():
        raise SystemExit("placeholder lock values are forbidden")
    lock_digest = sha256(LOCK_PATH)
    try:
        image_lock = json.loads(IMAGE_LOCK_PATH.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise SystemExit("image lock is not valid JSON") from exc
    require_keys(image_lock, {"schema", "supply_chain_lock_sha256", "platform", "oci_index_sha256", "platform_manifest_digest", "config_digest", "registry_digest"}, "image")
    if image_lock["schema"] != "edu-agent.nocturne-image-lock.v1" or image_lock["supply_chain_lock_sha256"] != lock_digest:
        raise SystemExit("image lock is not bound to the current input lock")
    if image_lock["platform"] != build["platform"] or image_lock["registry_digest"] is not None:
        raise SystemExit("image lock must describe an offline OCI output for the locked platform")
    require_digest(image_lock["oci_index_sha256"], "OCI index")
    require_digest(image_lock["platform_manifest_digest"], "OCI platform manifest", prefixed=True)
    require_digest(image_lock["config_digest"], "OCI config", prefixed=True)
    print(f"input_lock_sha256={lock_digest}")
    print(f"image_lock_sha256={sha256(IMAGE_LOCK_PATH)}")
    print(f"requirements_sha256={sha256(requirements)} distributions={len(distributions)}")
    return lock


def safe_extract(archive: Path, destination: Path) -> Path:
    with tarfile.open(archive, "r:gz") as bundle:
        members = bundle.getmembers()
        roots = {Path(item.name).parts[0] for item in members if Path(item.name).parts}
        if len(roots) != 1:
            raise SystemExit("source archive has an unexpected root")
        root = next(iter(roots))
        for item in members:
            parts = Path(item.name).parts
            if item.islnk() or item.issym() or ".." in parts:
                raise SystemExit("source archive contains unsafe paths")
        bundle.extractall(destination, filter="data")
    return destination / root


def _output_destination(destination: Path) -> Path:
    output = Path(os.path.abspath(OUTPUT))
    candidate = Path(os.path.abspath(destination))
    if candidate == output or not candidate.is_relative_to(output):
        raise SystemExit("fetch destination must be a child directory of deploy/nocturne/output")
    if output.exists():
        output_info = output.lstat()
        if stat.S_ISLNK(output_info.st_mode) or not stat.S_ISDIR(output_info.st_mode):
            raise SystemExit("output root must be a non-symlink directory")
    else:
        output.mkdir(mode=0o755)
    current = output
    for part in candidate.relative_to(output).parts:
        current = current / part
        try:
            info = current.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(info.st_mode):
            raise SystemExit("fetch destination cannot contain symlinks")
        if current != candidate and not stat.S_ISDIR(info.st_mode):
            raise SystemExit("fetch destination parent must be a directory")
    return candidate


def remove_tree(path: Path) -> None:
    try:
        shutil.rmtree(path)
    except FileNotFoundError:
        return
    except OSError as exc:
        raise SystemExit(f"unable to remove generated directory: {path.name}") from exc


def _prepare_fetch_destination(destination: Path) -> Path:
    destination = _output_destination(destination)
    marker = destination / OUTPUT_MARKER
    if destination.exists():
        info = destination.lstat()
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise SystemExit("fetch destination must be a non-symlink directory")
        try:
            marker_info = marker.lstat()
            marker_content = marker.read_text(encoding="ascii")
        except (OSError, UnicodeError) as exc:
            raise SystemExit("refusing to delete an output directory without the agent marker") from exc
        if not stat.S_ISREG(marker_info.st_mode) or stat.S_ISLNK(marker_info.st_mode) or marker_content != OUTPUT_MARKER_CONTENT:
            raise SystemExit("refusing to delete an output directory without the agent marker")
        remove_tree(destination)
    destination.mkdir(parents=True)
    marker.write_text(OUTPUT_MARKER_CONTENT, encoding="ascii")
    return destination


def fetch_context(destination: Path) -> Path:
    destination = _prepare_fetch_destination(destination)
    lock = verify_lock()
    archive = destination / "source.tar.gz"
    with urllib.request.urlopen(lock["upstream"]["source_archive_url"], timeout=60) as response, archive.open("wb") as output:
        shutil.copyfileobj(response, output)
    if sha256(archive) != lock["upstream"]["source_archive_sha256"]:
        raise SystemExit("source archive SHA-256 mismatch")
    unpack = destination / "unpack"
    unpack.mkdir()
    source = safe_extract(archive, unpack)
    app = destination / "app"
    shutil.copytree(source / "backend", app)
    for overlay in (ROOT / "overlay").iterdir():
        if overlay.is_file():
            shutil.copy2(overlay, app / overlay.name)
    shutil.copy2(ROOT / "requirements.lock", destination / "requirements.lock")
    shutil.copy2(ROOT / "build-requirements.lock", destination / "build-requirements.lock")
    shutil.copy2(ROOT / "Dockerfile", destination / "Dockerfile")
    archive.unlink()
    remove_tree(unpack)
    epoch = lock["build"]["source_date_epoch"]
    for path in destination.rglob("*"):
        if not path.is_symlink():
            os.utime(path, (epoch, epoch))
    print(f"source_archive_sha256={lock['upstream']['source_archive_sha256']}")
    print(f"context={destination}")
    return destination


def download_verified(url: str, destination: Path, expected_sha256: str, label: str) -> Path:
    destination.parent.mkdir(parents=True, exist_ok=True)
    if destination.exists():
        if destination.is_file() and not destination.is_symlink() and hmac.compare_digest(sha256(destination), expected_sha256):
            return destination
        raise SystemExit(f"cached {label} does not match the supply-chain lock")
    temporary = destination.with_name(destination.name + ".partial")
    temporary.unlink(missing_ok=True)
    last_error: Exception | None = None
    for attempt in range(3):
        digest = hashlib.sha256()
        try:
            with urllib.request.urlopen(url, timeout=180) as response, temporary.open("xb") as output:
                for chunk in iter(lambda: response.read(1024 * 1024), b""):
                    digest.update(chunk)
                    output.write(chunk)
            if not hmac.compare_digest(digest.hexdigest(), expected_sha256):
                raise SystemExit(f"{label} SHA-256 mismatch")
            temporary.replace(destination)
            return destination
        except SystemExit:
            temporary.unlink(missing_ok=True)
            raise
        except Exception as exc:
            last_error = exc
            temporary.unlink(missing_ok=True)
            if attempt == 2:
                break
    raise SystemExit(f"failed to download {label} after 3 attempts") from last_error


def provision_buildx(lock: dict, docker_config: Path) -> tuple[list[str], dict[str, str]]:
    cached = OUTPUT / "toolchain" / f"docker-buildx-{lock['build']['buildx_version']}-linux-amd64"
    download_verified(
        lock["build"]["buildx_linux_amd64_url"],
        cached,
        lock["build"]["buildx_linux_amd64_sha256"],
        "Buildx artifact",
    )
    plugin = docker_config / "cli-plugins" / "docker-buildx"
    plugin.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(cached, plugin)
    plugin.chmod(0o755)
    env = dict(os.environ, DOCKER_CONFIG=str(docker_config))
    docker = ["docker", "--config", str(docker_config)]
    version = subprocess.run(docker + ["buildx", "version"], text=True, capture_output=True, env=env)
    if version.returncode or not re.search(rf"\b{re.escape(lock['build']['buildx_version'])}\b", version.stdout):
        raise SystemExit(f"downloaded Buildx must report {lock['build']['buildx_version']}")
    return docker, env


def read_json_file(path: Path, label: str) -> tuple[bytes, dict]:
    try:
        data = path.read_bytes()
        value = json.loads(data)
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise SystemExit(f"{label} is not valid JSON") from exc
    if not isinstance(value, dict):
        raise SystemExit(f"{label} must be a JSON object")
    return data, value


def decode_json_object(data: bytes, label: str) -> dict:
    try:
        value = json.loads(data)
    except (UnicodeError, json.JSONDecodeError) as exc:
        raise SystemExit(f"{label} is not valid JSON") from exc
    if not isinstance(value, dict):
        raise SystemExit(f"{label} must be a JSON object")
    return value


def blob(layout: Path, digest: str) -> tuple[Path, bytes]:
    algorithm, value = digest.split(":", 1)
    path = layout / "blobs" / algorithm / value
    data = path.read_bytes()
    if hashlib.sha256(data).hexdigest() != value:
        raise SystemExit(f"OCI blob digest mismatch: {digest}")
    return path, data


def verify_oci(layout: Path) -> dict:
    lock = verify_lock()
    _, layout_metadata = read_json_file(layout / "oci-layout", "OCI layout metadata")
    if layout_metadata.get("imageLayoutVersion") != "1.0.0":
        raise SystemExit("invalid OCI layout")
    index_data, index = read_json_file(layout / "index.json", "OCI index")
    if len(index.get("manifests", [])) != 1:
        raise SystemExit("OCI layout must contain one platform manifest")
    descriptor = index["manifests"][0]
    _, manifest_data = blob(layout, descriptor["digest"])
    manifest = decode_json_object(manifest_data, "OCI manifest")
    _, config_data = blob(layout, manifest["config"]["digest"])
    config = decode_json_object(config_data, "OCI config")
    for layer in manifest.get("layers", []):
        blob(layout, layer["digest"])
    if (config.get("os"), config.get("architecture")) != ("linux", "amd64"):
        raise SystemExit("OCI platform mismatch")
    labels = config.get("config", {}).get("Labels", {})
    for key, value in lock["image_contract"]["labels"].items():
        if labels.get(key) != value:
            raise SystemExit(f"OCI label mismatch: {key}")
    if labels.get("org.edu-agent.nocturne.supply-chain-lock-sha256") != sha256(LOCK_PATH):
        raise SystemExit("OCI supply-chain lock label mismatch")
    if config.get("config", {}).get("Cmd") != lock["image_contract"]["entrypoint"]:
        raise SystemExit("OCI command mismatch")
    result = {"oci_index_sha256": hashlib.sha256(index_data).hexdigest(), "platform_manifest_digest": descriptor["digest"], "config_digest": manifest["config"]["digest"], "registry_digest": None}
    _, expected = read_json_file(IMAGE_LOCK_PATH, "image lock")
    for key in ("oci_index_sha256", "platform_manifest_digest", "config_digest", "registry_digest"):
        if result[key] != expected.get(key):
            raise SystemExit(f"OCI output does not match image lock: {key}")
    print(json.dumps(result, sort_keys=True))
    print("registry_digest=not_generated")
    return result


def build() -> None:
    lock = verify_lock()
    version = subprocess.run(["docker", "version", "--format", "{{.Server.Version}}"], text=True, capture_output=True)
    if version.returncode:
        raise SystemExit("Docker daemon unavailable; OCI image was not generated")
    context = fetch_context(OUTPUT / "build-context")
    layout = OUTPUT / "oci-layout"
    if layout.is_symlink():
        raise SystemExit("OCI output cannot be a symlink")
    remove_tree(layout)

    builder = f"edu-agent-nocturne-{os.getpid()}"
    with tempfile.TemporaryDirectory(prefix="nocturne-docker-config-") as config_dir:
        docker_config = Path(config_dir)
        docker, env = provision_buildx(lock, docker_config)
        created = False
        try:
            subprocess.run(
                docker
                + [
                    "buildx",
                    "create",
                    "--name",
                    builder,
                    "--driver",
                    "docker-container",
                    "--driver-opt",
                    f"image={lock['build']['buildkit_image']}",
                    "--use",
                ],
                env=env,
                check=True,
                text=True,
            )
            created = True
            inspect = subprocess.run(docker + ["buildx", "inspect", builder, "--bootstrap"], text=True, capture_output=True, env=env, check=True)
            if not re.search(rf"BuildKit version:\s+{re.escape(lock['build']['buildkit_version'])}\b", inspect.stdout):
                raise SystemExit(f"builder must report BuildKit {lock['build']['buildkit_version']}")
            if lock["build"]["buildkit_image"] not in inspect.stdout:
                raise SystemExit("builder driver image does not match the input lock")
            container = f"buildx_buildkit_{builder}0"
            container_image = subprocess.run(docker + ["inspect", "--format", "{{.Config.Image}}", container], text=True, capture_output=True, env=env, check=True).stdout.strip()
            if container_image != lock["build"]["buildkit_image"]:
                raise SystemExit("running builder image digest does not match the input lock")
            command = docker + ["buildx", "build", "--builder", builder, "--platform", lock["build"]["platform"], "--no-cache", "--provenance=false", "--sbom=false", "--build-arg", f"SOURCE_DATE_EPOCH={lock['build']['source_date_epoch']}", "--build-arg", f"SUPPLY_CHAIN_LOCK_SHA256={sha256(LOCK_PATH)}", "--output", f"type=oci,dest={layout},tar=false,rewrite-timestamp=true", str(context)]
            subprocess.run(command, env=env, check=True)
        finally:
            if created:
                subprocess.run(docker + ["buildx", "rm", "--force", builder], env=env, check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    verify_oci(layout)


def test() -> None:
    verify_lock()
    context = fetch_context(OUTPUT / "contract-context")
    venv = OUTPUT / "contract-venv"
    remove_tree(venv)
    subprocess.run(["uv", "venv", "--python", "3.12", str(venv)], check=True)
    python = venv / "bin" / "python"
    subprocess.run(["uv", "pip", "install", "--python", str(python), "--require-hashes", "-r", str(ROOT / "build-requirements.lock")], check=True)
    subprocess.run(["uv", "pip", "install", "--python", str(python), "--require-hashes", "--no-build-isolation", "-r", str(ROOT / "requirements.lock")], check=True)
    subprocess.run(["uv", "pip", "install", "--python", str(python), "--require-hashes", "-r", str(REPO / "contracttests/nocturne/requirements.lock")], check=True)
    env = dict(os.environ, NOCTURNE_SOURCE_DIR=str(context / "app"))
    subprocess.run([str(python), "-m", "pytest", "-q", str(REPO / "contracttests/nocturne")], env=env, check=True)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("command", choices=("verify-lock", "fetch", "test", "build", "verify-oci"))
    parser.add_argument("path", nargs="?")
    args = parser.parse_args()
    if args.command == "verify-lock": verify_lock()
    elif args.command == "fetch": fetch_context(Path(args.path) if args.path else OUTPUT / "source-context")
    elif args.command == "test": test()
    elif args.command == "build": build()
    else: verify_oci(Path(args.path) if args.path else OUTPUT / "oci-layout")


if __name__ == "__main__":
    main()
