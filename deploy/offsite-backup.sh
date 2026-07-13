#!/usr/bin/env bash
set -Eeuo pipefail

# Default action is a read-only audit. Set OFFSITE_TARGET and SSH_KEY_FILE in
# the environment; do not hardcode hostnames, IPs, passwords, or tokens here.

ACTION="${1:-audit}"
APPLY="${2:-}"
SOURCE_DIR="${SOURCE_DIR:-/root/ledger-pg-backups}"
SSH_KEY_FILE="${SSH_KEY_FILE:-}"
OFFSITE_TARGET="${OFFSITE_TARGET:-}"
RETENTION_DAYS="${RETENTION_DAYS:-30}"

die() { printf '[offsite-backup] ERROR: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

audit() {
  [[ -d "$SOURCE_DIR" ]] || die "source directory missing: $SOURCE_DIR"
  find "$SOURCE_DIR" -maxdepth 1 -type f -name '*.sql.gz' -printf '%TY-%Tm-%Td %TH:%TM %s %f\n' | sort
  local count
  count="$(find "$SOURCE_DIR" -maxdepth 1 -type f -name '*.sql.gz' | wc -l)"
  (( count >= 2 )) || die "expected at least one bot dump and one watcher dump"
  while IFS= read -r file; do gzip -t "$file" || die "invalid gzip: $file"; done < <(find "$SOURCE_DIR" -maxdepth 1 -type f -name '*.sql.gz')
  printf '[offsite-backup] local audit passed; no changes were made\n'
}

push() {
  [[ "$APPLY" == "--apply" ]] || die "push requires --apply"
  need rsync; need ssh
  audit
  [[ -n "$OFFSITE_TARGET" ]] || die "set OFFSITE_TARGET=user@private-host:/restricted/path"
  [[ -r "$SSH_KEY_FILE" ]] || die "set readable SSH_KEY_FILE"
  rsync -a --protect-args --partial --checksum \
    -e "ssh -i $SSH_KEY_FILE -o BatchMode=yes -o StrictHostKeyChecking=yes" \
    "$SOURCE_DIR/" "$OFFSITE_TARGET/"
  printf '[offsite-backup] upload completed; remote deletion is intentionally disabled\n'
}

case "$ACTION" in
  audit) audit ;;
  push) push ;;
  *) die "usage: $0 {audit|push} [--apply]" ;;
esac
