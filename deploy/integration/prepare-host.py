#!/usr/bin/env python3
"""Prepare only the dedicated integration namespace on the authorized ECS.

Run as root after reviewing the script. Secrets stay in root-only files;
existing VMS/IdP databases, accounts, configurations and services are untouched.
"""
import grp
import json
import os
from pathlib import Path
import pwd
import secrets
import subprocess
import sys

DOMAIN = "rustdesk.viget.net"
ACCOUNT = "rustdesk-integration"
DATABASE = "rustdesk_integration"
OPT = Path("/opt/rustdesk-integration")
DATA = Path("/var/lib/rustdesk-integration")
ETC = Path("/etc/rustdesk-integration")
MARKER = "# managed: rustdesk-integration\n"


def run(args, data=None):
    result = subprocess.run(args, input=data, text=True, capture_output=True)
    if result.returncode:
        # SQL input and generated configuration can contain secrets. Do not
        # relay subprocess stderr/arguments through the remote command log.
        raise RuntimeError(f"operation failed: {Path(args[0]).name}")
    return result.stdout.strip()


def sql(statement):
    return run(["runuser", "-u", "postgres", "--", "psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-d", "postgres"], statement)


def write(path, content, mode=0o644, gid=0):
    if path.is_symlink():
        raise RuntimeError(f"refuse symlink target: {path}")
    temp = path.with_name(path.name + ".rustdesk-tmp")
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(content)
        os.chmod(temp, mode)
        os.chown(temp, 0, gid)
        os.replace(temp, path)
    finally:
        if temp.exists():
            temp.unlink()


def no_symlink_parents(path):
    for parent in (path, *path.parents):
        if parent.is_symlink():
            raise RuntimeError("dedicated namespace traverses a symlink")


def main():
    if os.geteuid() != 0:
        raise RuntimeError("root required")
    # Check the complete namespace before chmod/chown or reading credentials.
    for path in (OPT, OPT / "api/releases", OPT / "server", ETC, ETC / "credentials.json",
                 DATA, DATA / "api/logs", DATA / "server", DATA / "acme",
                 Path("/etc/nginx/sites-available/rustdesk-integration")):
        no_symlink_parents(path)
    os.umask(0o077)
    try:
        user = pwd.getpwnam(ACCOUNT)
    except KeyError:
        run(["useradd", "--system", "--user-group", "--home-dir", str(DATA), "--shell", "/usr/sbin/nologin", ACCOUNT])
        user = pwd.getpwnam(ACCOUNT)
    if user.pw_uid == 0 or user.pw_dir != str(DATA) or user.pw_shell != "/usr/sbin/nologin":
        raise RuntimeError("existing service account does not match the dedicated namespace")
    gid = grp.getgrnam(ACCOUNT).gr_gid
    expected = {path: 0 for path in (OPT, OPT / "api", OPT / "api/releases", OPT / "server", ETC, DATA, DATA / "acme")}
    expected.update({path: user.pw_uid for path in (DATA / "api", DATA / "api/logs", DATA / "server")})
    for path, owner_uid in expected.items():
        if path.exists() and (not path.is_dir() or path.stat().st_uid != owner_uid or path.stat().st_mode & 0o022):
            raise RuntimeError("existing namespace directory ownership or permissions mismatch")
    for path in (OPT, OPT / "api", OPT / "api/releases", OPT / "server", ETC, DATA):
        path.mkdir(parents=True, exist_ok=True)
        os.chmod(path, 0o750 if path in (ETC, DATA) else 0o755)
        os.chown(path, 0, gid)
    for path in (DATA / "api", DATA / "api/logs", DATA / "server"):
        path.mkdir(parents=True, exist_ok=True)
        os.chmod(path, 0o700 if path == DATA / "server" else 0o750)
        os.chown(path, user.pw_uid, gid)
    (DATA / "acme").mkdir(exist_ok=True)
    os.chmod(DATA, 0o755)  # Nginx can traverse only the public ACME subdirectory.
    os.chmod(DATA / "acme", 0o755)

    credentials = ETC / "credentials.json"
    if credentials.exists():
        if not credentials.is_file() or credentials.stat().st_uid != 0 or credentials.stat().st_mode & 0o077:
            raise RuntimeError("owned root-only credentials file required")
        values = json.loads(credentials.read_text())
    else:
        if sql(f"SELECT 1 FROM pg_roles WHERE rolname='{DATABASE}';") or sql(f"SELECT 1 FROM pg_database WHERE datname='{DATABASE}';"):
            raise RuntimeError("dedicated role/database already exists without owned credentials; review required")
        values = {"database_password": secrets.token_hex(32), "jwt_key": secrets.token_hex(32)}
        write(credentials, json.dumps(values) + "\n", 0o600)
    for key in ("database_password", "jwt_key"):
        value = values.get(key)
        if not isinstance(value, str) or len(value) != 64 or any(c not in "0123456789abcdef" for c in value):
            raise RuntimeError("invalid owned credential format")
    # The generated password consists only of hex; it never appears in argv.
    if not sql(f"SELECT 1 FROM pg_roles WHERE rolname='{DATABASE}';"):
        sql(f"CREATE ROLE {DATABASE} LOGIN CONNECTION LIMIT 10 PASSWORD '{values['database_password']}';")
    if not sql(f"SELECT 1 FROM pg_database WHERE datname='{DATABASE}';"):
        sql(f"CREATE DATABASE {DATABASE} OWNER {DATABASE} ENCODING 'UTF8';")
    owner = sql(f"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='{DATABASE}';")
    if owner != DATABASE:
        raise RuntimeError("dedicated database owner mismatch")

    config = {
        "lang": "en",
        "app": {"register": False, "register-status": 1, "web-client": 0, "show-swagger": 0,
                "token-expire": "168h", "auth-timeout": "10s", "web-sso": True,
                "disable-pwd-login": False, "captcha-threshold": 3, "ban-threshold": 0},
        "admin": {"title": "RustDesk Integration", "hello": "Integration environment"},
        "gin": {"api-addr": "127.0.0.1:21114", "mode": "release", "trust-proxy": "127.0.0.1",
                "resources-path": str(OPT / "api/current/resources")},
        "gorm": {"type": "postgresql", "max-idle-conns": 2, "max-open-conns": 8},
        "postgresql": {"host": "127.0.0.1", "port": "5432", "user": DATABASE,
                       "password": values["database_password"], "dbname": DATABASE,
                       "sslmode": "disable", "time-zone": "UTC"},
        "rustdesk": {"id-server": DOMAIN, "relay-server": DOMAIN + ":21117",
                     "api-server": "https://" + DOMAIN, "key-file": str(DATA / "server/id_ed25519.pub"), "personal": 1},
        "jwt": {"key": values["jwt_key"], "expire-duration": "168h"},
        "cache": {"type": "memory"},
        "logger": {"path": str(DATA / "api/logs/api.log"), "level": "info", "report-caller": False},
        "proxy": {"enable": False}, "ldap": {"enable": False},
    }
    # JSON is valid YAML and avoids adding a deployment-time YAML dependency.
    write(ETC / "config.yaml", json.dumps(config, indent=2) + "\n", 0o640, gid)

    available = Path("/etc/nginx/sites-available/rustdesk-integration")
    enabled = Path("/etc/nginx/sites-enabled/rustdesk-integration")
    if available.exists() and not available.read_text().startswith(MARKER):
        raise RuntimeError("refuse replacing unowned Nginx configuration")
    if not available.exists():
        write(available, MARKER + f"""server {{
    listen 80;
    server_name {DOMAIN};
    location ^~ /.well-known/acme-challenge/ {{ root {DATA}/acme; }}
    location / {{ return 503; }}
}}
""")
    if enabled.is_symlink():
        if enabled.resolve() != available:
            raise RuntimeError("Nginx enable link points elsewhere")
    elif enabled.exists():
        raise RuntimeError("Nginx enable target already exists")
    else:
        enabled.symlink_to(available)
    run(["nginx", "-t"])
    run(["systemctl", "reload", "nginx"])
    print("Dedicated account, directories, PostgreSQL role/database, private configuration and HTTP ACME vhost prepared.")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
