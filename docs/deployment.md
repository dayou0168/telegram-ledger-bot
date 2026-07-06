# Server Deployment

This bot can run with either Docker Compose or systemd. Docker Compose is the simpler first deployment path.

## Before Deploying

Create the Telegram bot in BotFather and disable privacy mode:

```text
/newbot
/setprivacy -> choose bot -> Disable
```

The `.env` value `TELEGRAM_API_BASE` must be a service root only:

```env
TELEGRAM_API_BASE=https://api.telegram.org
```

If using a self-hosted Telegram Bot API server:

```env
TELEGRAM_API_BASE=http://telegram-bot-api:8081
```

Do not include `/bot<TOKEN>/sendMessage`; the bot appends that path itself.

## Option A: Docker Compose

On an Ubuntu server:

```bash
sudo apt update
sudo apt install -y git docker.io docker-compose-plugin
sudo systemctl enable --now docker
```

Upload or clone the project to the server, then:

```bash
cd /opt/ledger-bot
cp .env.example .env
nano .env
mkdir -p data
docker compose up -d --build
docker compose logs -f ledger-bot
```

Minimum `.env`:

```env
TELEGRAM_BOT_TOKEN=123456:replace-me
TELEGRAM_BOT_USERNAME=your_bot_username
TELEGRAM_API_BASE=https://api.telegram.org
BOT_DB_PATH=data/ledger_bot.db
BOT_TIMEZONE=Asia/Shanghai
BOT_HOST_USER_ID=123456789
DEFAULT_OPERATOR_USER_IDS=
BOT_WORKER_THREADS=16
BOT_CONTROL_THREADS=6
BOT_CHAIN_THREADS=12
BOT_RATE_THREADS=1
BOT_BROADCAST_THREADS=4
BOT_QUERY_THREADS=4
BOT_NOTIFICATION_THREADS=6
BOT_HOST_CHECK_TTL_SECONDS=600
PUBLIC_BILL_BASE_URL=
PUBLIC_BILL_URL_TEMPLATE=
PUBLIC_BILL_BOT_NAME=LEDGER_BOT
BILL_WEB_ENABLED=1
BILL_WEB_HOST=0.0.0.0
BILL_WEB_PORT=8080
BILL_WEB_TOKEN=
ADMIN_WEB_TOKEN=
TRONSCAN_API_BASE=https://apilist.tronscanapi.com/api
TRONGRID_API_KEY=
TRON_POLL_INTERVAL_SECONDS=1
TRONSCAN_GLOBAL_SCAN_PAGES=1
TRON_ADDRESS_BACKFILL_SECONDS=60
P2P_RATE_API_BASE=https://p2p.army/api/fapi
P2P_RATE_FRONT_API=NextVOF2Ozuh36mW0TCv
P2P_RATE_MARKET=okx
P2P_RATE_FIAT_UNIT=CNY
P2P_RATE_ASSET=USDT
P2P_RATE_TRADE_METHODS=aliPay
P2P_RATE_REFRESH_SECONDS=60
P2P_RATE_CACHE_TTL_SECONDS=180
```

Send `我的ID` to the bot in private chat to get your Telegram ID, then put it in `BOT_HOST_USER_ID`. The bot has exactly one host. This value is required: if that host is not in a group, the bot leaves that group automatically. Sending `开始` only activates accounting; it does not promote the sender. Default operators are managed only by maintainers through `DEFAULT_OPERATOR_USER_IDS`; they can invite the bot into groups, record in any group, and use private `群发广播` / `分组广播`, but they cannot keep the bot in a group without the host present and cannot become group owner by sending `开始`.

Useful commands:

```bash
docker compose restart ledger-bot
docker compose logs -f ledger-bot
docker compose down
```

## Option A1: BaoTa Docker Compose

If you use the prebuilt GitHub Container Registry image, BaoTa only needs a Compose script. You do not need to upload source code.

Use the repository file `docker-compose.ghcr.yml`. It is the BaoTa-ready Compose script, with Chinese comments for every setting and placeholders for the values that must be filled.

Docker will create the persistent volume automatically. The SQLite database stays in `ledger_bot_data`.

## Built-in bill website

The image serves bill pages on port `8080` when `BILL_WEB_ENABLED=1`.

For Baota/Nginx:

1. Point your domain, for example `bot.example.com`, to the server IP.
2. Create a Baota website for that domain and apply for an SSL certificate.
3. Add a reverse proxy from the HTTPS site to `http://127.0.0.1:8080`.
4. Set:

```env
PUBLIC_BILL_BASE_URL=https://bot.example.com
PUBLIC_BILL_URL_TEMPLATE=
BILL_WEB_ENABLED=1
BILL_WEB_PORT=8080
BILL_WEB_TOKEN=replace-with-random-text
ADMIN_WEB_TOKEN=replace-with-admin-password
```

The Telegram bill button then opens `/bill/{chat_id}/{day}` on your own domain. If `BILL_WEB_TOKEN` is empty, the bill URL is public; for production use, set a random token. The admin backend is `/admin`; set `ADMIN_WEB_TOKEN` as the login password.

The built-in web server also supports legacy-style `/day_xxb.php` links. Use `created_at=YYYY-MM-DD` to open a historical bill, and append `download=excel` to download the current bill window as an `.xlsx` file named like `账单_日期_群名_时间戳.xlsx`.

The source-build path below is only needed if you want BaoTa to build the image on your server.

Recommended project path:

```text
/www/wwwroot/ledger-bot
```

Upload the whole project folder to that path, then create:

```text
/www/wwwroot/ledger-bot/.env
```

Minimum `.env`:

```env
TELEGRAM_BOT_TOKEN=123456:replace-me
TELEGRAM_BOT_USERNAME=your_bot_username
TELEGRAM_API_BASE=https://api.telegram.org
BOT_DB_PATH=data/ledger_bot.db
BOT_TIMEZONE=Asia/Shanghai
BOT_HOST_USER_ID=123456789
DEFAULT_OPERATOR_USER_IDS=
BOT_WORKER_THREADS=16
BOT_CONTROL_THREADS=6
BOT_CHAIN_THREADS=12
BOT_RATE_THREADS=1
BOT_BROADCAST_THREADS=4
BOT_QUERY_THREADS=4
BOT_NOTIFICATION_THREADS=6
BOT_HOST_CHECK_TTL_SECONDS=600
PUBLIC_BILL_BASE_URL=
PUBLIC_BILL_URL_TEMPLATE=
PUBLIC_BILL_BOT_NAME=LEDGER_BOT
BILL_WEB_ENABLED=1
BILL_WEB_HOST=0.0.0.0
BILL_WEB_PORT=8080
BILL_WEB_TOKEN=
ADMIN_WEB_TOKEN=

TRONSCAN_API_BASE=https://apilist.tronscanapi.com/api
TRONGRID_API_KEY=
TRON_USDT_CONTRACT=TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t
TRON_POLL_INTERVAL_SECONDS=1
TRON_INITIAL_LOOKBACK_MINUTES=15
TRONSCAN_GLOBAL_SCAN_PAGES=1
TRON_ADDRESS_BACKFILL_SECONDS=60
P2P_RATE_API_BASE=https://p2p.army/api/fapi
P2P_RATE_FRONT_API=NextVOF2Ozuh36mW0TCv
P2P_RATE_MARKET=okx
P2P_RATE_FIAT_UNIT=CNY
P2P_RATE_ASSET=USDT
P2P_RATE_TRADE_METHODS=aliPay
P2P_RATE_REFRESH_SECONDS=60
P2P_RATE_CACHE_TTL_SECONDS=180
```

In BaoTa:

1. Open Docker -> Compose.
2. Create a new Compose project.
3. Set project directory to `/www/wwwroot/ledger-bot`.
4. Use the repository `docker-compose.yml`.
5. Start the project.
6. Open logs and look for:

```text
Ledger bot is running.
```

This bot does not need a website port mapping. It uses Telegram long polling and outbound HTTP requests to Telegram/Tronscan.

If BaoTa asks for Compose content directly, paste:

```yaml
services:
  ledger-bot:
    build: .
    container_name: ledger-bot
    restart: unless-stopped
    env_file:
      - .env
    volumes:
      - ./data:/app/data
```

Restart after code/config changes:

```text
Docker -> Compose -> ledger-bot -> Restart
```

Back up this file regularly:

```text
/www/wwwroot/ledger-bot/data/ledger_bot.db
```

## Option B: systemd

Install Python:

```bash
sudo apt update
sudo apt install -y python3.12 python3.12-venv git
sudo useradd --system --create-home --shell /usr/sbin/nologin ledgerbot
sudo mkdir -p /opt/ledger-bot
sudo chown ledgerbot:ledgerbot /opt/ledger-bot
```

Copy the project into `/opt/ledger-bot`, then:

```bash
cd /opt/ledger-bot
sudo -u ledgerbot python3.12 -m venv .venv
sudo -u ledgerbot .venv/bin/pip install -r requirements.txt
sudo -u ledgerbot cp .env.example .env
sudo -u ledgerbot nano .env
sudo cp deploy/systemd/ledger-bot.service /etc/systemd/system/ledger-bot.service
sudo systemctl daemon-reload
sudo systemctl enable --now ledger-bot
sudo journalctl -u ledger-bot -f
```

Restart after code or config changes:

```bash
sudo systemctl restart ledger-bot
sudo journalctl -u ledger-bot -f
```

## Telegram Group Setup

Add the bot to a test group and promote it to administrator. Recommended permissions:

- Send messages
- Pin messages, if using accounting pin
- Restrict members, if using `上课` / `下课`

Then test in the group:

```text
开始
设置汇率10
设置费率3
+1000
+1000/9.2
下发100U
+0
显示账单
```

Permission check: the configured `BOT_HOST_USER_ID` must be in the group. A default operator may send `开始`, but that does not make the default operator the highest permission user.

Private chat tests:

```text
/start
我的ID
地址监听
只有宿主、默认操作人、一级操作人和下级操作人可用；点击地址监听面板里的按钮添加地址、备注和最小提醒金额
群列表
点击群发广播/分组广播按钮选择目标并发送测试内容
点击后台管理进入 /admin 管理分组、权限和广播替换
```

## Backup

Back up the SQLite database regularly:

```bash
cp /opt/ledger-bot/data/ledger_bot.db /opt/ledger-bot/data/ledger_bot.$(date +%F-%H%M%S).db
```

For Docker Compose deployments, the database is in:

```text
/opt/ledger-bot/data/ledger_bot.db
```
