#!/usr/bin/env python3
"""Install a verified API/resource bundle, switch the owned link, and check health."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request

ROOT = Path("/opt/rustdesk-integration/api")
UNIT = "rustdesk-integration-api.service"
SYSTEMD = Path("/etc/systemd/system")


def owned_file(path):
    if path.is_symlink() or (path.exists() and (not path.is_file() or path.stat().st_uid != 0 or path.stat().st_mode & 0o022)):
        raise RuntimeError("expected an owned regular deployment file")


def atomic_write(path, content, mode=0o600):
    owned_file(path)
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            os.chmod(temporary, mode)
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
    try:
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def run(args):
    result = subprocess.run(args, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(f"operation failed: {Path(args[0]).name}")


def sha256(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def main():
    if os.geteuid() != 0 or len(sys.argv) != 4:
        raise RuntimeError("usage (root): install-api.py bundle.tar.gz commit_sha bundle_sha256")
    bundle, revision, checksum = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
    if not re.fullmatch(r"[0-9a-f]{40}", revision) or not re.fullmatch(r"[0-9a-f]{64}", checksum):
        raise RuntimeError("invalid revision/checksum")
    if sha256(bundle) != checksum:
        raise RuntimeError("bundle checksum mismatch")
    os.umask(0o022)
    releases = ROOT / "releases"
    if not releases.is_dir() or releases.is_symlink() or releases.stat().st_uid != 0:
        raise RuntimeError("prepared root-owned releases directory required")
    target = releases / revision
    if target.is_symlink():
        raise RuntimeError("release must not be a symlink")
    if target.exists():
        if json.loads((target / "release.json").read_text())["bundle_sha256"] != checksum:
            raise RuntimeError("existing release differs")
    else:
        stage = Path(tempfile.mkdtemp(prefix=".staging-", dir=releases))
        with tarfile.open(bundle, "r:gz") as archive:
            for member in archive.getmembers():
                path = Path(member.name)
                if path.is_absolute() or ".." in path.parts or not (member.isfile() or member.isdir()):
                    raise RuntimeError("unsafe bundle entry")
                if not path.parts or path.parts[0] not in {"rustdesk-api", "REVISION", "GO_VERSION", "resources", "LICENSE", "deployment"}:
                    raise RuntimeError("unexpected bundle entry")
            archive.extractall(stage, filter="data")
        if (stage / "REVISION").read_text().strip() != revision:
            raise RuntimeError("artifact revision mismatch")
        binary = stage / "rustdesk-api"
        for path in stage.rglob("*"):
            os.chmod(path, 0o755 if path.is_dir() else 0o644)
        os.chmod(stage, 0o755)
        os.chmod(binary, 0o755)
        manifest = {"commit": revision, "bundle_sha256": checksum,
                    "binary_sha256": sha256(binary),
                    "go_version": (stage / "GO_VERSION").read_text().strip()}
        (stage / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")
        stage.rename(target)
    current = ROOT / "current"
    if current.exists() and not current.is_symlink():
        raise RuntimeError("refuse replacing non-symlink current")
    if current.is_symlink() and not current.resolve().is_relative_to(releases):
        raise RuntimeError("current points outside owned releases")
    previous = current.resolve() if current.is_symlink() else None
    destination = SYSTEMD / UNIT
    source = target / "deployment" / UNIT
    owned_file(destination)
    if destination.exists() and "Description=RustDesk integration API\n" not in destination.read_text():
        raise RuntimeError("refuse replacing unowned service unit")
    # Unit syntax and ownership must be checked before the running process's
    # resources-path (current/resources) can change.
    run(["systemd-analyze", "verify", str(source)])
    state_path = ROOT / "deployment-state.json"
    owned_file(state_path)
    state = {"commit": revision, "previous_release": str(previous) if previous else None,
             "phase": "prepared", "rollback_policy": "manual after checking database migration compatibility"}
    if state_path.exists():
        saved = json.loads(state_path.read_text())
        if saved.get("commit") == revision and previous == target:
            state["previous_release"] = saved.get("previous_release")
    backup = ROOT / ("previous-unit-" + revision + ".service")
    if destination.exists() and not backup.exists():
        atomic_write(backup, destination.read_text())
    state["previous_unit"] = str(backup) if backup.exists() else None
    atomic_write(state_path, json.dumps(state, indent=2) + "\n")
    temp_link = ROOT / (".current-" + revision)
    if temp_link.exists() or temp_link.is_symlink():
        raise RuntimeError("temporary current link already exists")
    temp_link.symlink_to(target)
    os.replace(temp_link, current)
    state["phase"] = "switched; migration may have run; manual recovery required on failure"
    atomic_write(state_path, json.dumps(state, indent=2) + "\n")
    atomic_write(destination, source.read_text(), 0o644)
    run(["systemctl", "daemon-reload"])
    run(["systemctl", "enable", UNIT])
    run(["systemctl", "restart", UNIT])
    for _ in range(30):
        try:
            with urllib.request.urlopen("http://127.0.0.1:21114/api/", timeout=2) as response:
                if response.status == 200:
                    state["phase"] = "ready"
                    atomic_write(state_path, json.dumps(state, indent=2) + "\n")
                    print(json.dumps({"api_ready": True, "commit": revision, "previous_release": state["previous_release"]}))
                    return
        except OSError:
            time.sleep(0.5)
    raise RuntimeError("API readiness failed; inspect the private application log")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
