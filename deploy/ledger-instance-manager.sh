#!/usr/bin/env bash
set -Eeuo pipefail

ACTION="${1:-}"
INSTANCE_FILE="${2:-}"
APPLY="${3:-}"
SHARED_FILE="${LEDGER_SHARED_ENV:-/etc/telegram-ledger/shared.env}"

log() { printf '[ledger-instance] %s\n' "$*"; }
die() { printf '[ledger-instance] ERROR: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing command: $1"; }

usage() {
  cat <<'EOF'
Usage:
  ledger-instance-manager.sh plan    /root/new-bot.env
  ledger-instance-manager.sh install /root/new-bot.env --apply
  ledger-instance-manager.sh status  /root/new-bot.env

The shared host file defaults to /etc/telegram-ledger/shared.env.
Override it with LEDGER_SHARED_ENV=/path/to/shared.env.
EOF
}

require_root() { [[ $EUID -eq 0 ]] || die 'run as root'; }
require_apply() { [[ "$APPLY" == '--apply' ]] || die 'install requires --apply'; }

load_env_file() {
  local file="$1"
  [[ -r "$file" ]] || die "cannot read env file: $file"
  # These files are root-owned deployment inputs, not untrusted user uploads.
  set -a
  # shellcheck disable=SC1090
  source "$file"
  set +a
}

validate_inputs() {
  [[ "${INSTANCE:-}" =~ ^[a-z0-9][a-z0-9-]{1,30}$ ]] || die 'INSTANCE must use 2-31 lowercase letters, digits, or hyphens'
  [[ -n "${TELEGRAM_BOT_TOKEN:-}" ]] || die 'TELEGRAM_BOT_TOKEN is required'
  [[ "${TELEGRAM_BOT_USERNAME:-}" =~ ^[A-Za-z0-9_]{5,32}$ ]] || die 'invalid TELEGRAM_BOT_USERNAME'
  [[ "${BOT_HOST_USER_ID:-}" =~ ^[1-9][0-9]+$ ]] || die 'BOT_HOST_USER_ID must be a positive integer'
  [[ "${PUBLIC_DOMAIN:-}" =~ ^[A-Za-z0-9.-]+$ && "$PUBLIC_DOMAIN" == *.* ]] || die 'invalid PUBLIC_DOMAIN'
}

validate_host() {
  [[ -n "${POSTGRES_PASSWORD:-}" && "$POSTGRES_PASSWORD" != replace_* ]] || die 'set POSTGRES_PASSWORD in shared.env'
  [[ "${POSTGRES_USER:-}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || die 'invalid POSTGRES_USER'
  [[ "${POSTGRES_HOST:-}" =~ ^[A-Za-z0-9_.:-]+$ ]] || die 'invalid POSTGRES_HOST'
  [[ "${POSTGRES_PORT:-}" =~ ^[0-9]+$ ]] || die 'invalid POSTGRES_PORT'
  [[ -x "$PG_BIN_DIR/psql" && -x "$PG_BIN_DIR/createdb" && -x "$PG_BIN_DIR/dropdb" ]] || die "PostgreSQL tools not found in $PG_BIN_DIR"
  [[ -r "$WATCHER_ENV_FILE" ]] || die "watcher env not found: $WATCHER_ENV_FILE"
  [[ -r "$BOT_DEFAULTS_FILE" ]] || die "bot defaults not found: $BOT_DEFAULTS_FILE"
  [[ -x "$NGINX_BIN" && -x "$NGINX_RELOAD" ]] || die 'BaoTa Nginx commands are not executable'
}

derive() {
  POSTGRES_USER="${POSTGRES_USER:-ledger}"
  POSTGRES_HOST="${POSTGRES_HOST:-127.0.0.1}"
  POSTGRES_PORT="${POSTGRES_PORT:-5432}"
  PG_BIN_DIR="${PG_BIN_DIR:-/www/server/pgsql/bin}"
  BOT_IMAGE="${BOT_IMAGE:-ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.13}"
  BOT_DEFAULTS_FILE="${BOT_DEFAULTS_FILE:-/etc/telegram-ledger/config/bot-defaults.env}"
  WATCHER_URL="${WATCHER_URL:-http://127.0.0.1:8090}"
  WATCHER_DATABASE_NAME="${WATCHER_DATABASE_NAME:-ledgerchainwatcher}"
  WATCHER_ENV_FILE="${WATCHER_ENV_FILE:-/etc/ledger-chain-watcher/env}"
  WATCHER_SERVICE="${WATCHER_SERVICE:-ledger-chain-watcher}"
  COMPOSE_ROOT="${COMPOSE_ROOT:-/www/server/panel/data/compose}"
  NGINX_VHOST_ROOT="${NGINX_VHOST_ROOT:-/www/server/panel/vhost/nginx}"
  NGINX_CERT_ROOT="${NGINX_CERT_ROOT:-/www/server/panel/vhost/cert}"
  WWW_ROOT="${WWW_ROOT:-/www/wwwroot}"
  NGINX_BIN="${NGINX_BIN:-/www/server/nginx/sbin/nginx}"
  NGINX_RELOAD="${NGINX_RELOAD:-/etc/init.d/nginx}"

  PROJECT_NAME="zhuanfa-${INSTANCE}"
  CONTAINER_NAME="$PROJECT_NAME"
  BOT_ID="$TELEGRAM_BOT_USERNAME"
  DATABASE_NAME="${DATABASE_NAME:-ledgerbot${INSTANCE//-/}}"
  [[ "$DATABASE_NAME" =~ ^[a-z][a-z0-9]{1,62}$ ]] || die 'DATABASE_NAME must contain lowercase letters and digits only'
  COMPOSE_DIR="$COMPOSE_ROOT/$PROJECT_NAME"
  COMPOSE_FILE="$COMPOSE_DIR/docker-compose.yaml"
  RUNTIME_DIR="/etc/telegram-ledger/instances"
  RUNTIME_FILE="$RUNTIME_DIR/$INSTANCE.env"
  VHOST_FILE="$NGINX_VHOST_ROOT/$PUBLIC_DOMAIN.conf"
  CERT_DIR="$NGINX_CERT_ROOT/$PUBLIC_DOMAIN"
  SITE_ROOT="$WWW_ROOT/$PUBLIC_DOMAIN"
}

find_free_port() {
  local port
  if [[ -n "${ADMIN_WEB_PORT:-}" ]]; then
    [[ "$ADMIN_WEB_PORT" =~ ^[0-9]+$ ]] || die 'ADMIN_WEB_PORT must be numeric'
    if ss -ltnH "sport = :$ADMIN_WEB_PORT" | grep -q .; then
      die "ADMIN_WEB_PORT is already in use: $ADMIN_WEB_PORT"
    fi
    return
  fi
  for port in $(seq 8081 8999); do
    if ! ss -ltnH "sport = :$port" | grep -q .; then
      ADMIN_WEB_PORT="$port"
      return
    fi
  done
  die 'no free admin port found in 8081-8999'
}

urlencode() {
  python3 - "$1" <<'PY'
import sys
from urllib.parse import quote
print(quote(sys.argv[1], safe=''))
PY
}

show_plan() {
  cat <<EOF
instance=$INSTANCE
project=$PROJECT_NAME
container=$CONTAINER_NAME
database=$DATABASE_NAME
domain=$PUBLIC_DOMAIN
admin_port=${ADMIN_WEB_PORT:-auto}
compose=$COMPOSE_FILE
runtime_env=$RUNTIME_FILE
watcher_bot_id=$BOT_ID
image=$BOT_IMAGE
advanced_defaults=$BOT_DEFAULTS_FILE
EOF
}

write_runtime() {
  local encoded_password watcher_secret session_secret admin_token database_url fallback_url
  encoded_password="$(urlencode "$POSTGRES_PASSWORD")"
  watcher_secret="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n')"
  session_secret="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n')"
  admin_token="${ADMIN_WEB_TOKEN:-}"
  database_url="postgres://${POSTGRES_USER}:${encoded_password}@${POSTGRES_HOST}:${POSTGRES_PORT}/${DATABASE_NAME}?sslmode=disable"
  fallback_url="postgres://${POSTGRES_USER}:${encoded_password}@${POSTGRES_HOST}:${POSTGRES_PORT}/${WATCHER_DATABASE_NAME}?sslmode=disable"

  install -d -m 0700 "$RUNTIME_DIR"
  umask 077
  cat >"$RUNTIME_FILE" <<EOF
TELEGRAM_BOT_TOKEN=$TELEGRAM_BOT_TOKEN
TELEGRAM_BOT_USERNAME=$TELEGRAM_BOT_USERNAME
BOT_HOST_USER_ID=$BOT_HOST_USER_ID
DEFAULT_OPERATOR_USER_IDS=${DEFAULT_OPERATOR_USER_IDS:-}
DATABASE_URL=$database_url
ADMIN_WEB_HOST=127.0.0.1
ADMIN_WEB_PORT=$ADMIN_WEB_PORT
ADMIN_SESSION_SECRET=$session_secret
ADMIN_WEB_TOKEN=$admin_token
PUBLIC_BILL_BASE_URL=https://$PUBLIC_DOMAIN
PUBLIC_BILL_URL_TEMPLATE=
PUBLIC_BILL_BOT_NAME=$TELEGRAM_BOT_USERNAME
CHAIN_WATCHER_URL=$WATCHER_URL
CHAIN_WATCHER_BOT_ID=$BOT_ID
CHAIN_WATCHER_SECRET=$watcher_secret
BOT_FALLBACK_INSTANCE_ID=${BOT_ID}-${CONTAINER_NAME}
BOT_FALLBACK_SHARED_DATABASE_URL=$fallback_url
EOF
  chmod 0600 "$RUNTIME_FILE"
  WATCHER_SECRET="$watcher_secret"
  ADMIN_TOKEN="$admin_token"
}

create_database() {
  if runuser -u postgres -- "$PG_BIN_DIR/psql" -d postgres -Atqc "SELECT 1 FROM pg_database WHERE datname='$DATABASE_NAME'" | grep -qx 1; then
    die "database already exists: $DATABASE_NAME"
  fi
  runuser -u postgres -- "$PG_BIN_DIR/createdb" -O "$POSTGRES_USER" "$DATABASE_NAME"
}

database_exists() {
  runuser -u postgres -- "$PG_BIN_DIR/psql" -d postgres -Atqc \
    "SELECT 1 FROM pg_database WHERE datname='$DATABASE_NAME'" | grep -qx 1
}

watcher_bot_exists() {
  WATCHER_ENV_FILE="$WATCHER_ENV_FILE" BOT_ID="$BOT_ID" python3 <<'PY'
import os
from pathlib import Path

path = Path(os.environ['WATCHER_ENV_FILE'])
bot_id = os.environ['BOT_ID']
for line in path.read_text().splitlines():
    if not line.startswith('CHAIN_WATCHER_BOTS='):
        continue
    raw = line.split('=', 1)[1].strip()
    if len(raw) >= 2 and raw[0] == raw[-1] and raw[0] in "'\"":
        raw = raw[1:-1]
    entries = [item.strip() for item in raw.split(',') if item.strip()]
    raise SystemExit(0 if any(item.split(':', 1)[0] == bot_id for item in entries) else 1)
raise SystemExit(1)
PY
}

register_watcher() {
  WATCHER_ENV_FILE="$WATCHER_ENV_FILE" BOT_ID="$BOT_ID" WATCHER_SECRET="$WATCHER_SECRET" python3 <<'PY'
import os
from pathlib import Path

path = Path(os.environ['WATCHER_ENV_FILE'])
bot_id = os.environ['BOT_ID']
secret = os.environ['WATCHER_SECRET']
lines = path.read_text().splitlines()
for index, line in enumerate(lines):
    if not line.startswith('CHAIN_WATCHER_BOTS='):
        continue
    raw = line.split('=', 1)[1].strip()
    quoted = len(raw) >= 2 and raw[0] == raw[-1] and raw[0] in "'\""
    if quoted:
        raw = raw[1:-1]
    entries = [item.strip() for item in raw.split(',') if item.strip()]
    if any(item.split(':', 1)[0] == bot_id for item in entries):
        raise SystemExit(f'watcher bot id already exists: {bot_id}')
    entries.append(f'{bot_id}:{secret}')
    value = ','.join(entries)
    lines[index] = f'CHAIN_WATCHER_BOTS="{value}"' if quoted else f'CHAIN_WATCHER_BOTS={value}'
    break
else:
    lines.append(f'CHAIN_WATCHER_BOTS={bot_id}:{secret}')
path.write_text('\n'.join(lines) + '\n')
os.chmod(path, 0o600)
PY
  systemctl restart "$WATCHER_SERVICE"
  systemctl is-active --quiet "$WATCHER_SERVICE" || die 'watcher failed after credential registration'
}

write_compose() {
  install -d -m 0700 "$COMPOSE_DIR"
  cat >"$COMPOSE_FILE" <<EOF
services:
  bot:
    image: $BOT_IMAGE
    container_name: $CONTAINER_NAME
    restart: unless-stopped
    network_mode: "host"
    env_file:
      - $BOT_DEFAULTS_FILE
      - $RUNTIME_FILE
    logging:
      driver: "json-file"
      options:
        max-size: "20m"
        max-file: "5"
EOF
  chmod 0600 "$COMPOSE_FILE"
  docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" config -q
}

write_nginx() {
  install -d -m 0755 "$CERT_DIR" "$SITE_ROOT/.well-known/acme-challenge"
  if [[ ! -s "$CERT_DIR/fullchain.pem" || ! -s "$CERT_DIR/privkey.pem" ]]; then
    openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 825 \
      -subj "/CN=$PUBLIC_DOMAIN" -addext "subjectAltName=DNS:$PUBLIC_DOMAIN" \
      -keyout "$CERT_DIR/privkey.pem" -out "$CERT_DIR/fullchain.pem" >/dev/null 2>&1
    chmod 0600 "$CERT_DIR/privkey.pem"
  fi
  cat >"$VHOST_FILE" <<EOF
server {
    listen 80;
    listen [::]:80;
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name $PUBLIC_DOMAIN;
    root $SITE_ROOT;
    ssl_certificate $CERT_DIR/fullchain.pem;
    ssl_certificate_key $CERT_DIR/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    location /.well-known/acme-challenge/ { root $SITE_ROOT; }
    location / {
        proxy_pass http://127.0.0.1:$ADMIN_WEB_PORT;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_set_header X-Forwarded-Host \$host;
        proxy_set_header X-Forwarded-Port \$server_port;
        proxy_connect_timeout 10s;
        proxy_send_timeout 600s;
        proxy_read_timeout 600s;
    }
    access_log /www/wwwlogs/$PUBLIC_DOMAIN.log;
    error_log /www/wwwlogs/$PUBLIC_DOMAIN.error.log;
}
EOF
  "$NGINX_BIN" -t
  "$NGINX_RELOAD" reload
}

preflight_install() {
  [[ ! -e "$COMPOSE_DIR" ]] || die "compose project already exists: $COMPOSE_DIR"
  [[ ! -e "$RUNTIME_FILE" ]] || die "runtime env already exists: $RUNTIME_FILE"
  [[ ! -e "$VHOST_FILE" ]] || die "Nginx site already exists: $VHOST_FILE"
  if [[ -e "$CERT_DIR" ]] && { [[ ! -s "$CERT_DIR/fullchain.pem" ]] || [[ ! -s "$CERT_DIR/privkey.pem" ]]; }; then
    die "certificate directory exists but is incomplete: $CERT_DIR"
  fi
  if docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
    die "container already exists: $CONTAINER_NAME"
  fi
  database_exists && die "database already exists: $DATABASE_NAME"
  watcher_bot_exists && die "watcher bot id already exists: $BOT_ID"
  find_free_port
}

rollback_install() {
  local exit_code="${1:-1}"
  trap - EXIT ERR
  set +e
  log "installation failed; rolling back resources created by this run"
  if [[ ${COMPOSE_CREATED:-0} -eq 1 ]]; then
    docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" down --remove-orphans >/dev/null 2>&1
  fi
  if [[ ${WATCHER_BACKUP_READY:-0} -eq 1 && -n "${WATCHER_BACKUP:-}" && -f "$WATCHER_BACKUP" ]]; then
    cp -a "$WATCHER_BACKUP" "$WATCHER_ENV_FILE"
    systemctl restart "$WATCHER_SERVICE" >/dev/null 2>&1
  fi
  if [[ ${VHOST_CREATED:-0} -eq 1 ]]; then
    rm -f -- "$VHOST_FILE"
    "$NGINX_BIN" -t >/dev/null 2>&1 && "$NGINX_RELOAD" reload >/dev/null 2>&1
  fi
  [[ ${COMPOSE_DIR_CREATED:-0} -eq 1 ]] && rm -rf -- "$COMPOSE_DIR"
  [[ ${RUNTIME_CREATED:-0} -eq 1 ]] && rm -f -- "$RUNTIME_FILE"
  [[ ${DATABASE_CREATED:-0} -eq 1 ]] && runuser -u postgres -- "$PG_BIN_DIR/dropdb" --if-exists "$DATABASE_NAME" >/dev/null 2>&1
  [[ ${CERT_DIR_CREATED:-0} -eq 1 ]] && rm -rf -- "$CERT_DIR"
  [[ ${SITE_ROOT_CREATED:-0} -eq 1 ]] && rm -rf -- "$SITE_ROOT"
  [[ -n "${WATCHER_BACKUP:-}" ]] && rm -f -- "$WATCHER_BACKUP"
  exit "$exit_code"
}

install_instance() {
  require_root
  require_apply
  for command in docker python3 openssl ss curl systemctl runuser; do need "$command"; done
  validate_host
  preflight_install

  DATABASE_CREATED=0
  RUNTIME_CREATED=0
  COMPOSE_DIR_CREATED=0
  COMPOSE_CREATED=0
  VHOST_CREATED=0
  CERT_DIR_CREATED=0
  SITE_ROOT_CREATED=0
  WATCHER_BACKUP_READY=0
  [[ -e "$CERT_DIR" ]] || CERT_DIR_CREATED=1
  [[ -e "$SITE_ROOT" ]] || SITE_ROOT_CREATED=1
  WATCHER_BACKUP=""
  trap 'rollback_install $?' EXIT

  log "creating database $DATABASE_NAME"
  create_database
  DATABASE_CREATED=1
  log 'generating runtime secrets and configuration'
  RUNTIME_CREATED=1
  write_runtime
  WATCHER_BACKUP="$(mktemp "${WATCHER_ENV_FILE}.install-${INSTANCE}.XXXXXX")"
  cp -a "$WATCHER_ENV_FILE" "$WATCHER_BACKUP"
  WATCHER_BACKUP_READY=1
  register_watcher
  COMPOSE_DIR_CREATED=1
  COMPOSE_CREATED=1
  write_compose
  VHOST_CREATED=1
  write_nginx
  docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" up -d
  for _ in $(seq 1 45); do
    if curl -fsS --max-time 3 "http://127.0.0.1:$ADMIN_WEB_PORT/health" >/dev/null; then
      log "installed successfully"
      printf 'admin_url=https://%s/admin\n' "$PUBLIC_DOMAIN"
      printf 'admin_password_file=%s\n' "$RUNTIME_FILE"
      printf 'compose_file=%s\n' "$COMPOSE_FILE"
      rm -f -- "$WATCHER_BACKUP"
      WATCHER_BACKUP=""
      trap - EXIT ERR
      return
    fi
    sleep 1
  done
  docker logs --tail 100 "$CONTAINER_NAME" >&2 || true
  die 'bot health check timed out'
}

status_instance() {
  require_root
  validate_host
  [[ -r "$RUNTIME_FILE" ]] || die "runtime env not found: $RUNTIME_FILE"
  load_env_file "$RUNTIME_FILE"
  docker inspect "$CONTAINER_NAME" --format 'container={{.Name}} image={{.Config.Image}} state={{.State.Status}} restarts={{.RestartCount}}'
  curl -fsS --max-time 5 "http://127.0.0.1:${ADMIN_WEB_PORT}/health"
  printf '\n'
  runuser -u postgres -- "$PG_BIN_DIR/psql" -d postgres -Atqc "SELECT datname, pg_size_pretty(pg_database_size(datname)) FROM pg_database WHERE datname='$DATABASE_NAME'"
}

[[ -n "$ACTION" && -n "$INSTANCE_FILE" ]] || { usage; exit 2; }
load_env_file "$SHARED_FILE"
load_env_file "$INSTANCE_FILE"
derive
validate_inputs

case "$ACTION" in
  plan) show_plan ;;
  install) install_instance ;;
  status) status_instance ;;
  *) usage; exit 2 ;;
esac
