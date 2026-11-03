#!/usr/bin/env bash
# Operator-only provisioning; run as the same non-root UID/GID as the service.
set -euo pipefail
umask 077

if [[ "$(uname -s)" != Linux || "$(id -u)" == 0 ]]; then
  printf 'BLOCKED: key provisioning requires Linux and the non-root service identity.\n' >&2
  exit 3
fi
if [[ $# != 1 || ! -d "$1" || -L "$1" ]]; then
  printf 'FAIL: provide one existing, private key directory (not a symlink).\n' >&2
  exit 1
fi
DIR="$1"
UID_NOW="$(id -u)"
GID_NOW="$(id -g)"

validate() {
  local file="$1" kind="$2" owner group mode bytes
  [[ ! -L "$file" ]] || { printf 'FAIL: symlinks are not provisioning targets.\n' >&2; return 1; }
  owner="$(stat -c %u -- "$file")"
  group="$(stat -c %g -- "$file")"
  mode="$(stat -c %a -- "$file")"
  [[ "$owner" == "$UID_NOW" && "$group" == "$GID_NOW" && "$mode" =~ ^[0-7]+$ ]] || {
    printf 'FAIL: directory and keys must belong to the service UID/GID.\n' >&2; return 1;
  }
  (( (8#$mode & 8#077) == 0 )) || { printf 'FAIL: group/other permissions must be empty.\n' >&2; return 1; }
  if [[ "$kind" == file ]]; then
    [[ -f "$file" && -r "$file" ]] || { printf 'FAIL: existing key must be a readable regular file.\n' >&2; return 1; }
    bytes="$(stat -c %s -- "$file")"
    (( bytes > 0 && bytes <= 4096 )) || { printf 'FAIL: existing key is empty or oversized; it was not changed.\n' >&2; return 1; }
  else
    [[ -r "$file" && -w "$file" && -x "$file" ]] || { printf 'FAIL: key directory is not usable by the service identity.\n' >&2; return 1; }
  fi
}

validate "$DIR" directory
# Validate all existing assets before creating any missing file. Never chmod/chown/delete old keys.
for name in master pepper admin; do
  if [[ -e "$DIR/$name" || -L "$DIR/$name" ]]; then validate "$DIR/$name" file; fi
done
for name in master pepper admin; do
  if [[ -e "$DIR/$name" || -L "$DIR/$name" ]]; then
    validate "$DIR/$name" file
    printf 'KEPT %s (existing bytes and permissions unchanged)\n' "$name"
    continue
  fi
  if [[ "$name" == admin ]]; then
    value="ak_$(dd if=/dev/urandom bs=32 count=1 status=none | od -An -tx1 | tr -d ' \n')"
    [[ "$value" =~ ^ak_[0-9a-f]{64}$ ]] || { printf 'FAIL: random generation failed.\n' >&2; exit 1; }
  else
    value="$(dd if=/dev/urandom bs=32 count=1 status=none | base64 -w0)"
    [[ "$value" =~ ^[A-Za-z0-9+/]{43}=$ ]] || { printf 'FAIL: random generation failed.\n' >&2; exit 1; }
  fi
  # noclobber rejects an intervening file creation. No retry can truncate somebody else's key.
  (set -o noclobber; printf '%s\n' "$value" > "$DIR/$name") || {
    printf 'FAIL: exclusive key creation failed; no existing key was overwritten.\n' >&2; exit 1;
  }
  unset value
  validate "$DIR/$name" file
  printf 'CREATED %s (0600, service UID/GID)\n' "$name"
done
