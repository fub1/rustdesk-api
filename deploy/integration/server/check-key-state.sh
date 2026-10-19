#!/usr/bin/env bash
set -euo pipefail

readonly DATA_DIR=/var/lib/rustdesk-integration/server
readonly PRIVATE_KEY="$DATA_DIR/id_ed25519"
readonly PUBLIC_KEY="$DATA_DIR/id_ed25519.pub"
mode="${1:-}"
fail() { printf 'RustDesk key preflight: %s\n' "$1" >&2; exit 1; }
[[ "$mode" == hbbs || "$mode" == hbbr ]] || fail 'expected hbbs or hbbr'
[[ -d "$DATA_DIR" && ! -L "$DATA_DIR" ]] || fail 'data directory is absent or symlinked'
[[ "$(realpath -e "$DATA_DIR")" == "$DATA_DIR" ]] || fail 'data directory traverses a symlink'
[[ "$(stat -c %u "$DATA_DIR")" == "$(id -u)" ]] || fail 'data directory is not owned by the service user'
data_permissions="$(stat -c %a "$DATA_DIR")"
(( (8#$data_permissions & 077) == 0 )) || fail 'data directory exposes private state to other users'

if [[ ! -e "$PRIVATE_KEY" && ! -L "$PRIVATE_KEY" && ! -e "$PUBLIC_KEY" && ! -L "$PUBLIC_KEY" ]]; then
  [[ "$mode" == hbbs ]] || fail 'hbbs has not generated the key pair yet; relay will retry'
  exit 0
fi

# Fail closed on a partial pair. The official 1.1.16 generator otherwise may
# create/truncate a public key before discovering an unreadable private key.
for key_file in "$PRIVATE_KEY" "$PUBLIC_KEY"; do
  [[ -f "$key_file" && ! -L "$key_file" && -r "$key_file" && -s "$key_file" ]] || fail 'existing key pair is incomplete, unreadable, empty, or symlinked'
done
[[ "$(stat -c %u "$PRIVATE_KEY")" == "$(id -u)" ]] || fail 'private key has an unexpected owner'
key_permissions="$(stat -c %a "$PRIVATE_KEY")"
(( (8#$key_permissions & 077) == 0 )) || fail 'private key permissions expose it to other users'

# Read secrets only through pipes; never print or copy private-key contents.
private_length="$(base64 --decode "$PRIVATE_KEY" 2>/dev/null | wc -c)" || fail 'private key is not valid base64'
public_length="$(base64 --decode "$PUBLIC_KEY" 2>/dev/null | wc -c)" || fail 'public key is not valid base64'
[[ "$private_length" -eq 64 && "$public_length" -eq 32 ]] || fail 'existing key lengths are invalid'
cmp -s <(base64 --decode "$PUBLIC_KEY" 2>/dev/null) <(base64 --decode "$PRIVATE_KEY" 2>/dev/null | tail -c 32) || fail 'existing public/private keys do not match'
