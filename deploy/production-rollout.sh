#!/usr/bin/env bash
set -Eeuo pipefail

# v2.4.8 production rollout helper. The default action is read-only preflight.
# Secrets and DSNs are read from existing runtime configuration or environment;
# never put them in this file or pass them as positional command arguments.

ACTION="${1:-preflight}"
APPLY="${2:-}"

BOT_CONTAINER="${BOT_CONTAINER:-ledger-bot-go}"
BOT_SERVICE="${BOT_SERVICE:-ledger-bot}"
BOT_COMPOSE_FILE="${BOT_COMPOSE_FILE:-}"
WATCHER_SERVICE="${WATCHER_SERVICE:-ledger-chain-watcher}"
WATCHER_BINARY="${WATCHER_BINARY:-/usr/local/bin/ledger-chain-watcher}"
WATCHER_ENV_FILE="${WATCHER_ENV_FILE:-/etc/ledger-chain-watcher/env}"
WATCHER_UNIT_FILE="${WATCHER_UNIT_FILE:-/etc/systemd/system/ledger-chain-watcher.service}"
WATCHER_URL="${WATCHER_URL:-http://127.0.0.1:8090}"
BACKUP_ROOT="${BACKUP_ROOT:-/root/ledger-upgrade-backups}"
OBSERVE_SECONDS="${OBSERVE_SECONDS:-600}"
PG_BIN_DIR="${PG_BIN_DIR:-}"

log() { printf '[rollout] %s\n' "$*"; }
die() { printf '[rollout] ERROR: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

require_apply() {
  [[ "$APPLY" == "--apply" ]] || die "this action changes production; rerun with: $ACTION --apply"
}

read_env_value() {
  local name="$1" file="$2"
  awk -F= -v key="$name" '$1 == key {sub(/^[^=]*=/, ""); sub(/^"/, ""); sub(/"$/, ""); print; exit}' "$file"
}

discover_pg_tools() {
  if [[ -z "$PG_BIN_DIR" ]]; then
    if command -v psql >/dev/null 2>&1; then
      PG_BIN_DIR="$(dirname "$(command -v psql)")"
    elif [[ -x /www/server/pgsql/bin/psql ]]; then
      PG_BIN_DIR=/www/server/pgsql/bin
    else
      die "PostgreSQL client tools not found; set PG_BIN_DIR"
    fi
  fi
  PSQL="$PG_BIN_DIR/psql"
  PG_DUMP="$PG_BIN_DIR/pg_dump"
  PG_RESTORE="$PG_BIN_DIR/pg_restore"
  [[ -x "$PSQL" && -x "$PG_DUMP" && -x "$PG_RESTORE" ]] || die "incomplete PostgreSQL client tools in $PG_BIN_DIR"
}

discover_compose() {
  if [[ -z "$BOT_COMPOSE_FILE" ]]; then
    BOT_COMPOSE_FILE="$(docker inspect "$BOT_CONTAINER" --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}')"
  fi
  [[ -n "$BOT_COMPOSE_FILE" && -f "$BOT_COMPOSE_FILE" ]] || die "BOT_COMPOSE_FILE not found"
}

discover_dsns() {
  if [[ -z "${BOT_DATABASE_URL:-}" ]]; then
    BOT_DATABASE_URL="$(docker inspect "$BOT_CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}' | awk -F= '$1=="DATABASE_URL" {sub(/^[^=]*=/, ""); print; exit}')"
  fi
  if [[ -z "${WATCHER_DATABASE_URL:-}" ]]; then
    WATCHER_DATABASE_URL="$(read_env_value CHAIN_WATCHER_DATABASE_URL "$WATCHER_ENV_FILE")"
  fi
  [[ -n "$BOT_DATABASE_URL" ]] || die "BOT_DATABASE_URL unavailable"
  [[ -n "$WATCHER_DATABASE_URL" ]] || die "WATCHER_DATABASE_URL unavailable"
}

admin_token() {
  read_env_value CHAIN_WATCHER_ADMIN_TOKEN "$WATCHER_ENV_FILE"
}

watcher_source_ready() {
  local token="$1" status
  status="$(curl -fsS --max-time 5 -H "Authorization: Bearer $token" "$WATCHER_URL/status")" || return 1
  grep -Eq '"source_ready"[[:space:]]*:[[:space:]]*true' <<<"$status" \
    && grep -Eq '"continuity_ready"[[:space:]]*:[[:space:]]*(true|false)' <<<"$status"
}

preflight() {
  for cmd in docker systemctl curl sha256sum awk grep stat; do need "$cmd"; done
  discover_pg_tools
  [[ $EUID -eq 0 ]] || die "run as root"
  [[ -x "$WATCHER_BINARY" ]] || die "watcher binary missing: $WATCHER_BINARY"
  [[ -r "$WATCHER_ENV_FILE" ]] || die "watcher env unreadable: $WATCHER_ENV_FILE"
  [[ -r "$WATCHER_UNIT_FILE" ]] || die "watcher unit unreadable: $WATCHER_UNIT_FILE"
  discover_compose
  discover_dsns

  log "read-only topology"
  docker inspect "$BOT_CONTAINER" --format 'bot={{.Name}} image={{.Config.Image}} image_id={{.Image}} state={{.State.Status}} restarts={{.RestartCount}} log={{json .HostConfig.LogConfig}}'
  systemctl show "$WATCHER_SERVICE" -p ActiveState -p SubState -p MainPID -p NRestarts -p ExecMainStartTimestamp --no-pager
  printf 'watcher_sha256='; sha256sum "$WATCHER_BINARY" | awk '{print $1}'
  printf 'compose_file=%s\n' "$BOT_COMPOSE_FILE"
  printf 'watcher_env_mode='; stat -c '%a %U:%G' "$WATCHER_ENV_FILE"

  curl -fsS --max-time 5 "$WATCHER_URL/healthz" >/dev/null || die "watcher healthz failed"
  local token
  token="$(admin_token)"
  [[ -n "$token" ]] || die "CHAIN_WATCHER_ADMIN_TOKEN is missing"
  watcher_source_ready "$token" || die "watcher status unavailable, source_ready is false, or continuity_ready is missing"

  PGDATABASE="$BOT_DATABASE_URL" "$PSQL" -Atqc 'select current_database(), now()' >/dev/null || die "bot database unavailable"
  PGDATABASE="$WATCHER_DATABASE_URL" "$PSQL" -Atqc 'select current_database(), now()' >/dev/null || die "watcher database unavailable"
  log "preflight passed; no changes were made"
}

backup_all() {
  discover_compose
  discover_dsns
  discover_pg_tools
  umask 077
  local dir="$BACKUP_ROOT/v2.4.8-$(date +%Y%m%d-%H%M%S)"
  install -d -m 0700 "$dir"
  log "creating verified backup: $dir"

  PGDATABASE="$BOT_DATABASE_URL" "$PG_DUMP" --format=custom --file="$dir/bot.dump"
  PGDATABASE="$WATCHER_DATABASE_URL" "$PG_DUMP" --format=custom --file="$dir/watcher.dump"
  "$PG_RESTORE" -l "$dir/bot.dump" >/dev/null
  "$PG_RESTORE" -l "$dir/watcher.dump" >/dev/null
  test -s "$dir/bot.dump" && test -s "$dir/watcher.dump"

  install -m 0600 "$BOT_COMPOSE_FILE" "$dir/bot-compose.before.yml"
  install -m 0600 "$WATCHER_ENV_FILE" "$dir/watcher.env.before"
  install -m 0644 "$WATCHER_UNIT_FILE" "$dir/watcher.service.before"
  install -m 0755 "$WATCHER_BINARY" "$dir/ledger-chain-watcher.before"
  docker inspect "$BOT_CONTAINER" >"$dir/bot.inspect.before.json"
  sha256sum "$dir"/* >"$dir/SHA256SUMS"
  chmod 0600 "$dir"/*
  printf '%s\n' "$dir"
}

wait_watcher() {
  local token="$1"
  for _ in $(seq 1 30); do
    if systemctl is-active --quiet "$WATCHER_SERVICE" \
      && curl -fsS --max-time 3 "$WATCHER_URL/healthz" >/dev/null \
      && watcher_source_ready "$token"; then
      return 0
    fi
    sleep 2
  done
  return 1
}

watcher_runtime_rollback() {
  local dir="$1"
  log "restoring watcher runtime files from $dir; bot remains unchanged"
  install -m 0755 "$dir/ledger-chain-watcher.before" "$WATCHER_BINARY"
  install -m 0600 "$dir/watcher.env.before" "$WATCHER_ENV_FILE"
  install -m 0644 "$dir/watcher.service.before" "$WATCHER_UNIT_FILE"
  systemctl daemon-reload
  systemctl restart "$WATCHER_SERVICE"
}

runtime_rollback() {
  local dir="$1"
  log "restoring runtime files from $dir; database schema remains forward-only"
  install -m 0755 "$dir/ledger-chain-watcher.before" "$WATCHER_BINARY"
  install -m 0600 "$dir/watcher.env.before" "$WATCHER_ENV_FILE"
  install -m 0644 "$dir/watcher.service.before" "$WATCHER_UNIT_FILE"
  install -m 0600 "$dir/bot-compose.before.yml" "$BOT_COMPOSE_FILE"
  systemctl daemon-reload
  systemctl restart "$WATCHER_SERVICE"
  docker compose -f "$BOT_COMPOSE_FILE" up -d --no-deps --force-recreate "$BOT_SERVICE"
}

upgrade() {
  require_apply
  preflight
  [[ -n "${NEW_WATCHER_BINARY:-}" && -f "$NEW_WATCHER_BINARY" ]] || die "set NEW_WATCHER_BINARY"
  [[ -n "${NEW_WATCHER_SHA256:-}" ]] || die "set NEW_WATCHER_SHA256"
  [[ -n "${NEW_BOT_IMAGE:-}" ]] || die "set NEW_BOT_IMAGE"
  [[ -n "${NEW_BOT_COMPOSE_FILE:-}" && -f "$NEW_BOT_COMPOSE_FILE" ]] || die "set NEW_BOT_COMPOSE_FILE"
  printf '%s  %s\n' "$NEW_WATCHER_SHA256" "$NEW_WATCHER_BINARY" | sha256sum -c -
  grep -Fq "image: $NEW_BOT_IMAGE" "$NEW_BOT_COMPOSE_FILE" || die "new Compose does not pin NEW_BOT_IMAGE"

  local dir token
  dir="$(backup_all | tail -1)"
  token="$(admin_token)"

  log "upgrading watcher first"
  install -m 0755 "$NEW_WATCHER_BINARY" "$WATCHER_BINARY.new"
  mv -f "$WATCHER_BINARY.new" "$WATCHER_BINARY"
  systemctl restart "$WATCHER_SERVICE"
  if ! wait_watcher "$token"; then
    journalctl -u "$WATCHER_SERVICE" -n 100 --no-pager >&2 || true
    runtime_rollback "$dir"
    die "watcher upgrade failed; runtime rollback attempted; schema was not downgraded"
  fi

  log "watcher healthy; upgrading bot"
  install -m 0600 "$NEW_BOT_COMPOSE_FILE" "$BOT_COMPOSE_FILE.new"
  mv -f "$BOT_COMPOSE_FILE.new" "$BOT_COMPOSE_FILE"
  if ! docker compose -f "$BOT_COMPOSE_FILE" pull "$BOT_SERVICE" \
    || ! docker compose -f "$BOT_COMPOSE_FILE" up -d --no-deps --force-recreate "$BOT_SERVICE"; then
    runtime_rollback "$dir"
    die "bot upgrade failed; runtime rollback attempted; schema was not downgraded"
  fi
  sleep 5
  [[ "$(docker inspect "$BOT_CONTAINER" --format '{{.State.Status}}')" == "running" ]] || {
    runtime_rollback "$dir"
    die "bot did not stay running; runtime rollback attempted"
  }
  log "runtime upgrade complete; backup=$dir"
  log "run acceptance and complete the real USDT transfer checklist before closing the window"
}

upgrade_watcher() {
  require_apply
  preflight
  [[ -n "${NEW_WATCHER_BINARY:-}" && -f "$NEW_WATCHER_BINARY" ]] || die "set NEW_WATCHER_BINARY"
  [[ -n "${NEW_WATCHER_SHA256:-}" ]] || die "set NEW_WATCHER_SHA256"
  printf '%s  %s\n' "$NEW_WATCHER_SHA256" "$NEW_WATCHER_BINARY" | sha256sum -c -

  local dir token
  dir="$(backup_all | tail -1)"
  token="$(admin_token)"

  log "upgrading watcher only; bot container remains unchanged"
  install -m 0755 "$NEW_WATCHER_BINARY" "$WATCHER_BINARY.new"
  mv -f "$WATCHER_BINARY.new" "$WATCHER_BINARY"
  systemctl restart "$WATCHER_SERVICE"
  if ! wait_watcher "$token"; then
    journalctl -u "$WATCHER_SERVICE" -n 100 --no-pager >&2 || true
    watcher_runtime_rollback "$dir"
    die "watcher upgrade failed; watcher-only runtime rollback attempted"
  fi
  log "watcher-only upgrade complete; bot was not recreated; backup=$dir"
  log "run acceptance and complete the real upstream timeout/cancellation checklist before closing the window"
}

rollback() {
  require_apply
  [[ -n "${ROLLBACK_DIR:-}" && -d "$ROLLBACK_DIR" ]] || die "set ROLLBACK_DIR"
  sha256sum -c "$ROLLBACK_DIR/SHA256SUMS"
  discover_compose
  runtime_rollback "$ROLLBACK_DIR"
  log "runtime rollback complete; PostgreSQL schema was intentionally not downgraded"
  log "if old binaries are not forward-compatible, redeploy the new binaries or restore both databases with an explicit data-loss decision"
}

acceptance() {
  preflight
  discover_dsns
  [[ -n "${EXPECTED_WATCHER_SHA256:-}" ]] || die "set EXPECTED_WATCHER_SHA256 from the Release"
  log "checking v2.4.8 watcher runtime and historical schema"
  local bot_schema watcher_schema image_id repo_digests actual_watcher_sha unauth_code
  bot_schema="$(PGDATABASE="$BOT_DATABASE_URL" "$PSQL" -Atqc "select count(*) from schema_migrations where version='2.4.4-broadcast-group-ownership'; select count(*) from schema_migrations where version='2.4.5-broadcast-group-owner-transfer'; select count(*) from schema_migrations where version='2.4.6-chain-gap-scheduler'; select count(*) from schema_migrations where version='2.4.7-chain-gap-convergence'; select count(*) from information_schema.columns where table_schema='public' and table_name='broadcast_groups' and column_name='owner_user_id'; select count(*) from information_schema.tables where table_schema='public' and table_name in ('broadcast_group_owner_repair_candidates','broadcast_group_audit_events'); select count(*) from information_schema.tables where table_schema='public' and table_name='broadcast_group_owner_transfer_events'; select count(*) from information_schema.tables where table_schema='public' and table_name='chain_watcher_gap_metric_minutes'; select count(*) from pg_indexes where schemaname='public' and indexname='idx_broadcast_groups_owner'; select count(*) from pg_indexes where schemaname='public' and indexname='idx_chain_watcher_gap_fair_claim'; select count(*) from pg_indexes where schemaname='public' and indexname in ('idx_broadcast_group_owner_transfer_group','idx_broadcast_group_owner_transfer_actor','idx_broadcast_group_owner_transfer_new_owner'); select count(*) from pg_trigger where tgname='trg_broadcast_group_owner_transfer_immutable' and not tgisinternal;")"
  [[ "$bot_schema" == $'1\n1\n1\n1\n1\n2\n1\n1\n1\n1\n3\n1' ]] || die "historical bot migration objects missing"
  watcher_schema="$(PGDATABASE="$WATCHER_DATABASE_URL" "$PSQL" -Atqc "select count(*) from schema_migrations where version='2.4.3'; select count(*) from schema_migrations where version='2.4.6-chain-gap-scheduler'; select count(*) from schema_migrations where version='2.4.7-chain-gap-convergence'; select count(*) from information_schema.columns where table_schema='public' and table_name='chain_watcher_gap_tasks' and column_name in ('head_event_id','retry_after'); select count(*) from information_schema.tables where table_schema='public' and table_name='chain_watcher_gap_metric_minutes'; select count(*) from pg_indexes where schemaname='public' and indexname in ('idx_chain_watcher_gap_claim','idx_chain_watcher_gap_window_overlap','idx_chain_watcher_gap_ready_claim','idx_chain_watcher_gap_fair_claim');")"
  [[ "$watcher_schema" == $'1\n1\n1\n2\n1\n4' ]] || die "historical watcher migration/columns/indexes missing"
  unauth_code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$WATCHER_URL/status")"
  [[ "$unauth_code" == "401" ]] || die "unauthenticated /status returned $unauth_code, want 401"
  image_id="$(docker inspect "$BOT_CONTAINER" --format '{{.Image}}')"
  repo_digests="$(docker image inspect "$image_id" --format '{{join .RepoDigests "\n"}}')"
  if [[ -n "${EXPECTED_BOT_REPO_DIGEST:-}" ]]; then
    grep -Fq "$EXPECTED_BOT_REPO_DIGEST" <<<"$repo_digests" || die "bot repo digest mismatch"
  else
    log "EXPECTED_BOT_REPO_DIGEST not set; bot image is intentionally unchanged for watcher-only v2.4.8"
  fi
  actual_watcher_sha="$(sha256sum "$WATCHER_BINARY" | awk '{print $1}')"
  [[ "$actual_watcher_sha" == "$EXPECTED_WATCHER_SHA256" ]] || die "watcher SHA256 mismatch"
  docker inspect "$BOT_CONTAINER" --format 'image={{.Config.Image}} image_id={{.Image}} state={{.State.Status}} restarts={{.RestartCount}}'
  systemctl show "$WATCHER_SERVICE" -p ActiveState -p MainPID -p NRestarts -p ExecMainStartTimestamp --no-pager
  printf 'watcher_sha256=%s\n' "$actual_watcher_sha"

  [[ "$OBSERVE_SECONDS" =~ ^[0-9]+$ ]] || die "OBSERVE_SECONDS must be numeric"
  (( OBSERVE_SECONDS >= 600 && OBSERVE_SECONDS <= 900 )) || die "OBSERVE_SECONDS must be 600..900"
  log "observing health and restart counters for $OBSERVE_SECONDS seconds"
  local token end pid restarts
  token="$(admin_token)"; end=$((SECONDS + OBSERVE_SECONDS))
  while (( SECONDS < end )); do
    curl -fsS --max-time 5 "$WATCHER_URL/healthz" >/dev/null
    watcher_source_ready "$token"
    pid="$(systemctl show "$WATCHER_SERVICE" -p MainPID --value)"
    restarts="$(systemctl show "$WATCHER_SERVICE" -p NRestarts --value)"
    log "watcher pid=$pid restarts=$restarts bot_restarts=$(docker inspect "$BOT_CONTAINER" --format '{{.RestartCount}}')"
    sleep 30
  done
  journalctl -u "$WATCHER_SERVICE" --since "15 minutes ago" --no-pager | grep -Ei 'panic|fatal|migration|decrypt|401|403|429|deadline|error' || true
  docker logs --since 15m "$BOT_CONTAINER" 2>&1 | grep -Ei 'panic|fatal|migration|conflict|409|error' || true
  log "automated acceptance complete; a real incoming and outgoing USDT transfer remains a manual acceptance item"
}

case "$ACTION" in
  preflight) preflight ;;
  backup) require_apply; preflight; backup_all ;;
  upgrade) upgrade ;;
  upgrade-watcher) upgrade_watcher ;;
  rollback) rollback ;;
  acceptance) acceptance ;;
  *) die "usage: $0 {preflight|backup|upgrade|upgrade-watcher|rollback|acceptance} [--apply]" ;;
esac
