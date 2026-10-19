"""Exercise release recovery without invoking systemd or a real listener.

Run as root in Linux (CI uses sudo) so production ownership checks also run.
All paths and external commands are isolated to a TemporaryDirectory.
"""
import importlib.util
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import hashlib


spec = importlib.util.spec_from_file_location("install_api", Path(__file__).with_name("install-api.py"))
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


@unittest.skipUnless(hasattr(os, "geteuid") and os.geteuid() == 0, "Linux root ownership checks required")
class InstallRecovery(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "api"
        self.releases = self.root / "releases"
        self.releases.mkdir(parents=True)
        self.systemd = Path(self.temp.name) / "systemd"
        self.systemd.mkdir()
        self.previous = self.releases / ("1" * 40)
        self.previous.mkdir()
        (self.root / "current").symlink_to(self.previous)
        self.unit = self.systemd / installer.UNIT
        self.unit.write_text("[Unit]\nDescription=RustDesk integration API\n# old unit\n")
        self.revision = "2" * 40
        self.bundle = Path(self.temp.name) / "bundle.tar.gz"
        entries = {"REVISION": self.revision, "GO_VERSION": "go version test",
                   "rustdesk-api": "not executed", "resources/test": "same revision",
                   "deployment/" + installer.UNIT: "[Unit]\nDescription=RustDesk integration API\n# new unit\n"}
        with tarfile.open(self.bundle, "w:gz") as archive:
            for name, value in entries.items():
                data = value.encode()
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))
        self.checksum = hashlib.sha256(self.bundle.read_bytes()).hexdigest()
        for replacement in (patch.object(installer, "ROOT", self.root),
                            patch.object(installer, "SYSTEMD", self.systemd),
                            patch.object(installer.sys, "argv", ["install-api.py", str(self.bundle), self.revision, self.checksum])):
            replacement.start()
            self.addCleanup(replacement.stop)

    def test_invalid_unit_never_switches_resources(self):
        def run(command):
            if command[0] == "systemd-analyze":
                raise RuntimeError("invalid unit")
            self.fail("no service operation permitted before validation")
        with patch.object(installer, "run", side_effect=run):
            with self.assertRaisesRegex(RuntimeError, "invalid unit"):
                installer.main()
        self.assertEqual((self.root / "current").resolve(), self.previous)
        self.assertIn("old unit", self.unit.read_text())

    def test_symlink_unit_rejected_without_touching_target(self):
        protected = Path(self.temp.name) / "other.service"
        protected.write_text("unrelated service")
        self.unit.unlink()
        self.unit.symlink_to(protected)
        with patch.object(installer, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "owned regular"):
                installer.main()
            run.assert_not_called()
        self.assertEqual(protected.read_text(), "unrelated service")
        self.assertEqual((self.root / "current").resolve(), self.previous)

    def test_failed_restart_preserves_recovery_record_and_retry(self):
        def run(command):
            if command[:2] == ["systemctl", "restart"]:
                raise RuntimeError("restart failed")
        with patch.object(installer, "run", side_effect=run):
            with self.assertRaisesRegex(RuntimeError, "restart failed"):
                installer.main()
        state = json.loads((self.root / "deployment-state.json").read_text())
        self.assertEqual(state["previous_release"], str(self.previous))
        self.assertIn("manual recovery", state["phase"])
        self.assertIn("old unit", Path(state["previous_unit"]).read_text())
        # Never automatically run an old binary against a possibly migrated DB.
        self.assertEqual((self.root / "current").resolve(), self.releases / self.revision)
        response = unittest.mock.MagicMock()
        response.__enter__.return_value.status = 200
        with patch.object(installer, "run"), patch.object(installer.urllib.request, "urlopen", return_value=response):
            installer.main()
        recovered = json.loads((self.root / "deployment-state.json").read_text())
        self.assertEqual(recovered["phase"], "ready")
        self.assertEqual(recovered["previous_release"], str(self.previous))
        self.assertIn("old unit", Path(recovered["previous_unit"]).read_text())


if __name__ == "__main__":
    unittest.main()
