"""Offline bootstrap recovery tests; use only synthetic credentials and HTTP."""
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import tempfile
import types
import unittest
from unittest import mock
import urllib.error


spec = importlib.util.spec_from_file_location("configure_provider", Path(__file__).with_name("configure-provider.py"))
configure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(configure)


class FakeAPI:
    def __init__(self, test):
        self.test = test
        self.password = "Seed1234"  # Synthetic fixture, never a deploy credential.
        self.calls = []
        self.change_failure = None
        self.login_failure = None
        self.wrong_password_code = 101

    def __call__(self, request, timeout):
        self.test.assertEqual(timeout, 15)
        self.test.assertEqual(request.get_header("Accept-language"), "en")
        body = json.loads(request.data)
        path = request.full_url.removeprefix(configure.BASE)
        self.calls.append((path, body))
        if path == "/login":
            if self.login_failure is not None:
                failure = self.login_failure
                if isinstance(failure, Exception):
                    raise failure
                return io.BytesIO(failure)
            if body["password"] != self.password:
                result = {"code": self.wrong_password_code, "message": "Username or password error.", "data": None}
            else:
                result = {"code": 0, "data": {"token": "synthetic-session"}}
        elif path == "/user/changeCurPwd":
            self.test.assertEqual(request.get_header("Api-token"), "synthetic-session")
            pending = json.loads(configure.PRIVATE.read_text())
            self.test.assertTrue(pending["bootstrap_pending"])
            self.test.assertEqual(pending["old_password"], self.password)
            self.test.assertEqual(pending["password"], body["new_password"])
            self.test.assertEqual(body["old_password"], self.password)
            failure = self.change_failure
            self.change_failure = None
            if failure == "before commit":
                raise urllib.error.HTTPError(request.full_url, 503, "unavailable", {}, None)
            self.password = body["new_password"]
            if failure == "response lost":
                raise urllib.error.URLError("synthetic response lost")
            result = {"code": 0, "data": None}
        else:
            raise AssertionError("unexpected synthetic API path")
        return io.BytesIO(json.dumps(result).encode())


class BootstrapRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="rustdesk-bootstrap-test-")
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name)
        self.private = root / "admin.json"
        self.log = root / "api.log"
        self.log.write_text("synthetic log Admin Password Is: Seed1234\n")
        self.log.chmod(0o600)
        patches = [
            mock.patch.object(configure, "PRIVATE", self.private),
            mock.patch.object(configure, "BOOTSTRAP_LOG", self.log),
            mock.patch.object(configure.pwd, "getpwnam", return_value=types.SimpleNamespace(pw_uid=os.geteuid())),
            mock.patch.object(configure.secrets, "token_hex", return_value="a" * 32),
        ]
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        self.api = FakeAPI(self)
        patch = mock.patch.object(configure.urllib.request, "urlopen", side_effect=self.api)
        patch.start()
        self.addCleanup(patch.stop)

    def pending(self, legacy=False):
        value = {"username": "admin", "password": "a" * 32, "bootstrap_pending": True}
        if not legacy:
            value["old_password"] = "Seed1234"
        configure.save(value)

    def assert_complete(self):
        result = json.loads(self.private.read_text())
        self.assertEqual(result, {"username": "admin", "password": "a" * 32, "token": "synthetic-session"})
        self.assertEqual(stat.S_IMODE(self.private.stat().st_mode), 0o600)

    def test_initial_change_failure_remains_recoverable(self):
        self.api.change_failure = "before commit"
        with self.assertRaises(urllib.error.HTTPError):
            configure.administrator_session()
        pending = json.loads(self.private.read_text())
        self.assertEqual(pending["old_password"], "Seed1234")
        self.assertEqual(self.api.password, "Seed1234")
        self.log.unlink()  # The new pending document is independently recoverable.
        self.assertEqual(configure.administrator_session(), "synthetic-session")
        self.assert_complete()
        self.assertEqual([body["password"] for path, body in self.api.calls if path == "/login"],
                         ["Seed1234", "a" * 32, "Seed1234", "a" * 32])

    def test_committed_change_with_lost_response_is_not_repeated(self):
        self.api.change_failure = "response lost"
        with self.assertRaises(urllib.error.URLError):
            configure.administrator_session()
        self.assertTrue(json.loads(self.private.read_text())["bootstrap_pending"])
        self.log.unlink()
        configure.administrator_session()
        self.assert_complete()
        self.assertEqual(sum(path == "/user/changeCurPwd" for path, _ in self.api.calls), 1)

    def test_pending_only_new_uses_private_bootstrap_log_after_explicit_rejection(self):
        self.pending(legacy=True)
        configure.administrator_session()
        self.assert_complete()
        self.assertEqual([path for path, _ in self.api.calls],
                         ["/login", "/login", "/user/changeCurPwd", "/login"])

    def test_pending_only_new_committed_does_not_read_old_log(self):
        self.pending(legacy=True)
        self.api.password = "a" * 32
        self.log.unlink()
        configure.administrator_session()
        self.assert_complete()
        self.assertEqual(len(self.api.calls), 1)

    def test_explicit_wrong_password_code_110_can_recover(self):
        self.pending()
        self.api.wrong_password_code = 110
        configure.administrator_session()
        self.assert_complete()

    def test_pending_save_failure_prevents_password_mutation(self):
        with mock.patch.object(configure.os, "replace", side_effect=OSError("synthetic replacement failure")):
            with self.assertRaises(OSError):
                configure.administrator_session()
        self.assertFalse(self.private.exists())
        self.assertEqual(self.api.password, "Seed1234")
        self.assertEqual([path for path, _ in self.api.calls], ["/login"])

    def test_completed_credentials_never_fall_back(self):
        configure.save({"username": "admin", "password": "a" * 32})
        with self.assertRaises(configure.WrongPassword):
            configure.administrator_session()
        self.assertEqual(len(self.api.calls), 1)

    def test_pending_does_not_fall_back_on_network_5xx_or_other_business_errors(self):
        failures = [
            urllib.error.URLError("synthetic network failure"),
            urllib.error.HTTPError(configure.BASE + "/login", 503, "unavailable", {}, None),
            b"not JSON",
            b"[]",
            b'{"code":101,"message":"Captcha error."}',
            b'{"code":110,"message":"Captcha error."}',
            b'{"code":101,"message":"User disabled."}',
            b'{"code":503,"message":"Username or password error."}',
        ]
        for failure in failures:
            with self.subTest(failure=type(failure).__name__):
                self.pending(legacy=True)
                previous = self.private.read_bytes()
                self.api.calls.clear()
                self.api.login_failure = failure
                with self.assertRaises(Exception):
                    configure.administrator_session()
                self.assertEqual(self.private.read_bytes(), previous)
                self.assertEqual(len(self.api.calls), 1)

    def test_old_login_failure_does_not_change_password(self):
        self.pending()
        self.api.password = "Another1"
        with self.assertRaises(configure.WrongPassword):
            configure.administrator_session()
        self.assertEqual([path for path, _ in self.api.calls], ["/login", "/login"])
        self.assertTrue(json.loads(self.private.read_text())["bootstrap_pending"])

    def test_legacy_pending_rejects_public_or_ambiguous_bootstrap_log(self):
        self.pending(legacy=True)
        self.log.chmod(0o644)
        with self.assertRaises(RuntimeError):
            configure.administrator_session()
        self.assertEqual(len(self.api.calls), 1)
        self.log.chmod(0o600)
        self.log.write_text("Admin Password Is: Seed1234\nAdmin Password Is: Other123\n")
        self.api.calls.clear()
        with self.assertRaises(RuntimeError):
            configure.administrator_session()
        self.assertEqual(len(self.api.calls), 1)

    def test_atomic_save_failure_preserves_complete_old_document(self):
        self.pending()
        previous = self.private.read_bytes()
        with mock.patch.object(configure.os, "replace", side_effect=OSError("synthetic replacement failure")):
            with self.assertRaises(OSError):
                configure.save({"username": "admin", "password": "new-synthetic"})
        self.assertEqual(self.private.read_bytes(), previous)
        self.assertEqual(list(self.private.parent.glob(".admin-*")), [])
        self.assertEqual(stat.S_IMODE(self.private.stat().st_mode), 0o600)

    def test_save_and_load_reject_symlink_or_public_credentials(self):
        self.private.symlink_to(self.log)
        with self.assertRaises(RuntimeError):
            configure.save({"password": "synthetic"})
        with self.assertRaises(OSError):
            configure.administrator_session()
        self.private.unlink()
        self.pending()
        self.private.chmod(0o644)
        with self.assertRaises(RuntimeError):
            configure.administrator_session()
        self.assertEqual(self.api.calls, [])


if __name__ == "__main__":
    unittest.main()
