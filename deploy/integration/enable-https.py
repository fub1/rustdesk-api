#!/usr/bin/env python3
"""Issue a certificate and enable only the owned RustDesk Nginx vhost."""
import os
from pathlib import Path
import subprocess
import sys

DOMAIN = "rustdesk.viget.net"
MARKER = "# managed: rustdesk-integration\n"
VHOST = Path("/etc/nginx/sites-available/rustdesk-integration")


def run(args):
    result = subprocess.run(args, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(f"operation failed: {Path(args[0]).name}")


def main():
    if (os.geteuid() != 0 or VHOST.is_symlink() or VHOST.stat().st_uid != 0
            or VHOST.stat().st_mode & 0o022 or not VHOST.read_text().startswith(MARKER)):
        raise RuntimeError("root and the prepared owned vhost required")
    os.umask(0o077)
    if sys.argv[1:] == ["--existing-certificate"]:
        run(["openssl", "x509", "-in", f"/etc/letsencrypt/live/{DOMAIN}/fullchain.pem",
             "-noout", "-checkend", "0", "-checkhost", DOMAIN])
    elif not sys.argv[1:]:
        accounts = list(Path("/etc/letsencrypt/accounts/acme-v02.api.letsencrypt.org/directory").glob("*/regr.json"))
        if len(accounts) != 1:
            raise RuntimeError("select an existing ACME account explicitly before continuing")
        run(["certbot", "certonly", "--webroot", "-w", "/var/lib/rustdesk-integration/acme",
             "-d", DOMAIN, "--cert-name", DOMAIN, "--non-interactive", "--keep-until-expiring",
             "--account", accounts[0].parent.name,
             "--deploy-hook", "/usr/sbin/nginx -t && /bin/systemctl reload nginx"])
    else:
        raise RuntimeError("usage: enable-https.py [--existing-certificate]")
    previous = VHOST.read_text()
    content = MARKER + """log_format rustdesk_integration '$remote_addr [$time_local] "$request_method $uri $server_protocol" $status $body_bytes_sent';
server {
    listen 80;
    server_name rustdesk.viget.net;
    access_log /var/log/nginx/rustdesk-integration-access.log rustdesk_integration;
    error_log /dev/null;
    location ^~ /.well-known/acme-challenge/ { root /var/lib/rustdesk-integration/acme; }
    location / { return 301 https://$host$request_uri; }
}
server {
    listen 443 ssl;
    server_name rustdesk.viget.net;
    ssl_certificate /etc/letsencrypt/live/rustdesk.viget.net/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/rustdesk.viget.net/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    access_log /var/log/nginx/rustdesk-integration-access.log rustdesk_integration;
    error_log /var/log/nginx/rustdesk-integration-error.log;
    client_max_body_size 16m;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
    proxy_read_timeout 30s;
    # Nginx error records cannot be formatted to redact query parameters.
    # Restrict suppression to the authorization-code callback; its application
    # failure class and status remain available in redacted API/access logs.
    location = /api/oidc/callback {
        error_log /dev/null;
        proxy_pass http://127.0.0.1:21114;
    }
    location / {
        proxy_pass http://127.0.0.1:21114;
    }
}
"""
    VHOST.write_text(content)
    os.chmod(VHOST, 0o644)
    try:
        run(["nginx", "-t"])
        run(["systemctl", "reload", "nginx"])
    except RuntimeError:
        VHOST.write_text(previous)
        os.chmod(VHOST, 0o644)
        run(["nginx", "-t"])
        run(["systemctl", "reload", "nginx"])
        raise
    print("Dedicated certificate and HTTPS reverse proxy enabled; existing vhosts retained.")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
