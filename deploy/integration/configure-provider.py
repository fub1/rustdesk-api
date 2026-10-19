#!/usr/bin/env python3
"""Configure the test RP through its authenticated API, never by editing users."""
import json
import os
from pathlib import Path
import pwd
import re
import secrets
import stat
import sys
import tempfile
import urllib.request

BASE = "http://127.0.0.1:21114/api/admin"
PRIVATE = Path("/etc/rustdesk-integration/admin.json")
BOOTSTRAP_LOG = Path("/var/lib/rustdesk-integration/api/logs/api.log")


class WrongPassword(RuntimeError):
    """Only the login API's explicit, English wrong-credential response."""


def api(path, body=None, token=None):
    headers = {"Content-Type": "application/json", "Accept-Language": "en"}
    if token:
        headers["api-token"] = token
    request = urllib.request.Request(BASE + path, data=json.dumps(body).encode() if body is not None else None, headers=headers)
    with urllib.request.urlopen(request, timeout=15) as response:
        result = json.load(response)
    if not isinstance(result, dict):
        raise RuntimeError("invalid authenticated API response")
    if path == "/login" and result.get("code") in (101, 110) and result.get("message") == "Username or password error.":
        raise WrongPassword("administrator username or password rejected")
    if result.get("code") != 0:
        raise RuntimeError("authenticated API operation failed: " + path)
    return result.get("data")


def login(password):
    result = api("/login", {"username": "admin", "password": password, "platform": "integration-bootstrap"})
    if not isinstance(result, dict) or not isinstance(result.get("token"), str) or not result["token"]:
        raise RuntimeError("administrator login returned no session")
    return result["token"]


def owned_text(path, owners):
    # Read only a regular, private file. Do not follow a substituted final link.
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "r") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid not in owners or stat.S_IMODE(info.st_mode) & 0o077:
            raise RuntimeError("private bootstrap file ownership or permissions invalid")
        return stream.read()


def bootstrap_password():
    service_uid = pwd.getpwnam("rustdesk-integration").pw_uid
    text = owned_text(BOOTSTRAP_LOG, {0, service_uid})
    matches = re.findall(r"Admin Password Is:\s+([a-zA-Z0-9]{8})\b", text)
    if len(matches) != 1:
        raise RuntimeError("unique initial administrator credential not found in private bootstrap log")
    return matches[0]


def save(value):
    # An interrupted save must leave either the old complete document or the
    # new complete document, with credentials private throughout replacement.
    parent = PRIVATE.parent
    info = parent.stat()
    if parent.resolve() != parent or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) & 0o022:
        raise RuntimeError("private credentials directory ownership or permissions invalid")
    if PRIVATE.exists() or PRIVATE.is_symlink():
        info = PRIVATE.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) & 0o077:
            raise RuntimeError("private credentials file ownership or permissions invalid")
    fd, name = tempfile.mkstemp(prefix=".admin-", dir=parent)
    temp = Path(name)
    try:
        with os.fdopen(fd, "w") as stream:
            os.fchmod(stream.fileno(), 0o600)
            json.dump(value, stream)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, PRIVATE)
        directory = os.open(parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        temp.unlink(missing_ok=True)


def administrator_session():
    if PRIVATE.exists() or PRIVATE.is_symlink():
        credentials = json.loads(owned_text(PRIVATE, {os.geteuid()}))
        if not isinstance(credentials, dict) or credentials.get("username") != "admin" or not isinstance(credentials.get("password"), str) or not credentials["password"]:
            raise RuntimeError("invalid private administrator credential format")
        password = credentials["password"]
        if credentials.get("bootstrap_pending") is not True:
            token = login(password)
            credentials["token"] = token
            save(credentials)
            return token
        # The previous change may have committed even if its response was lost.
        # Try the intended password first. Never fall back on transport/5xx,
        # malformed responses, captcha, disabled-account or other API failures.
        try:
            token = login(password)
        except WrongPassword:
            old = credentials.get("old_password")
            if old is None:
                # Compatibility with the earlier pending-only-new document.
                old = bootstrap_password()
            if not isinstance(old, str) or not old:
                raise RuntimeError("invalid private pending bootstrap credential")
            token = login(old)
            save({"username": "admin", "password": password, "old_password": old, "bootstrap_pending": True})
            api("/user/changeCurPwd", {"old_password": old, "new_password": password}, token)
            token = login(password)
    else:
        old = bootstrap_password()
        token = login(old)
        password = secrets.token_hex(16)
        # Save both sides before mutating: failed and committed changes are
        # recoverable on the next invocation without logging either password.
        save({"username": "admin", "password": password, "old_password": old, "bootstrap_pending": True})
        api("/user/changeCurPwd", {"old_password": old, "new_password": password}, token)
        token = login(password)
    save({"username": "admin", "password": password, "token": token})
    return token


def main():
    if os.geteuid() != 0 or PRIVATE.is_symlink():
        raise RuntimeError("root and an owned credentials path required")
    os.umask(0o077)
    token = administrator_session()
    provider = {"op": "creatury", "oauth_type": "oidc", "issuer": "https://vms.creatury.cn/idp",
                "client_id": "gLUkinS2_kNpV4UFS0LME8toyMR3VzhtpBpUH9_-5fI", "client_secret": "",
                "scopes": "openid,profile", "pkce_enable": True, "pkce_method": "S256", "auto_register": True}
    listed = api("/oauth/list?page=1&page_size=100", token=token)
    existing = [row for row in listed["list"] if row.get("op") == provider["op"]]
    if len(existing) > 1:
        raise RuntimeError("ambiguous provider rows")
    if existing:
        provider["id"] = existing[0]["id"]
        api("/oauth/update", provider, token)
    else:
        api("/oauth/create", provider, token)
    print(json.dumps({"admin_bootstrap": "private credential rotated and session verified",
                      "provider": provider["op"], "public_none": True, "pkce": "S256",
                      "auto_register": "ordinary local users only", "no_idp_users_modified": True}))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # urllib exceptions are status-only; API JSON/passwords/tokens are never printed.
        print(str(error), file=sys.stderr)
        sys.exit(1)
