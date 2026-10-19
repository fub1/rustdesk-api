#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly SOURCE_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RELEASE_DIR=/opt/rustdesk-integration/server/1.1.16
readonly DATA_DIR=/var/lib/rustdesk-integration/server
readonly HBBS_SHA256=374c821722b04f6d7ef063c78bc5b4db7109b8b9613508822d803e31fdc5b625
readonly HBBR_SHA256=3ad04953242db5a9a78d45d41d13b9c6ecff651881e3a7f594f8b36a321e2457
fail() { printf 'RustDesk installation: %s\n' "$1" >&2; exit 1; }
[[ $# -eq 0 ]] || fail 'this installer takes no arguments and never starts services'
[[ "$(id -u)" -eq 0 ]] || fail 'run as root'
[[ "$(uname -m)" == x86_64 ]] || fail 'official asset requires Linux x86_64'
getent passwd rustdesk-integration >/dev/null || fail 'service user must be prepared first'
getent group rustdesk-integration >/dev/null || fail 'service group must be prepared first'
[[ -d "$DATA_DIR" && ! -L "$DATA_DIR" ]] || fail 'service data directory must be prepared first'
[[ "$(realpath -e "$DATA_DIR")" == "$DATA_DIR" ]] || fail 'data directory traverses a symlink'

printf '%s  %s\n' "$HBBS_SHA256" "$SOURCE_DIR/bin/hbbs" "$HBBR_SHA256" "$SOURCE_DIR/bin/hbbr" | sha256sum --check --status || fail 'official binary digest mismatch'
for directory in /opt/rustdesk-integration /opt/rustdesk-integration/server "$RELEASE_DIR"; do
  [[ ! -L "$directory" ]] || fail 'installation directory is symlinked'
  if [[ -e "$directory" ]]; then
    [[ -d "$directory" && "$(stat -c %u "$directory")" -eq 0 ]] || fail 'installation directories must be root-owned'
    permissions="$(stat -c %a "$directory")"
    (( (8#$permissions & 022) == 0 )) || fail 'installation directory is writable by non-root users'
  else
    install -d -o root -g root -m 0755 "$directory"
  fi
done

install_preserving_existing() {
  local source="$1" destination="$2" permissions="$3"
  if [[ -e "$destination" || -L "$destination" ]]; then
    [[ -f "$destination" && ! -L "$destination" ]] || fail 'existing installation file is not a regular file'
    [[ "$(stat -c %u "$destination")" -eq 0 ]] || fail 'existing installation file is not root-owned'
    local existing_permissions
    existing_permissions="$(stat -c %a "$destination")"
    (( (8#$existing_permissions & 022) == 0 )) || fail 'existing installation file is writable by non-root users'
    cmp -s "$source" "$destination" || fail 'existing file differs; refusing an implicit binary/unit replacement'
    return
  fi
  install -o root -g root -m "$permissions" "$source" "$destination"
}

install_preserving_existing "$SOURCE_DIR/bin/hbbs" "$RELEASE_DIR/hbbs" 0755
install_preserving_existing "$SOURCE_DIR/bin/hbbr" "$RELEASE_DIR/hbbr" 0755
install_preserving_existing "$SOURCE_DIR/check-key-state.sh" "$RELEASE_DIR/check-key-state.sh" 0755
runuser -u rustdesk-integration -- "$RELEASE_DIR/check-key-state.sh" hbbs
"$RELEASE_DIR/hbbs" --version
"$RELEASE_DIR/hbbr" --version
install_preserving_existing "$SOURCE_DIR/rustdesk-integration-hbbs.service" /etc/systemd/system/rustdesk-integration-hbbs.service 0644
install_preserving_existing "$SOURCE_DIR/rustdesk-integration-hbbr.service" /etc/systemd/system/rustdesk-integration-hbbr.service 0644
systemd-analyze verify /etc/systemd/system/rustdesk-integration-hbbs.service /etc/systemd/system/rustdesk-integration-hbbr.service
systemctl daemon-reload
printf 'Installed official RustDesk 1.1.16; no service was enabled, started or restarted.\n'
printf 'Persistent public key path: %s/id_ed25519.pub\n' "$DATA_DIR"
