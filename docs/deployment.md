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
TRONGRID_API_BASE=https://api.trongrid.io
TRONGRID_API_KEY=your-trongrid-key
TRON_POLL_INTERVAL_SECONDS=5
P2P_RATE_API_BASE=https://p2p.army/api/fapi
P2P_RATE_FRONT_API=NextVOF2Ozuh36mW0TCv
P2P_RATE_MARKET=okx
P2P_RATE_FIAT_UNIT=CNY
P2P_RATE_ASSET=USDT
P2P_RATE_TRADE_METHODS=aliPay
```

Useful commands:

```bash
docker compose restart ledger-bot
docker compose logs -f ledger-bot
docker compose down
```

## Option A1: BaoTa Docker Compose

If you use the prebuilt GitHub Container Registry image, BaoTa only needs a Compose script. You do not need to upload source code.

Paste this Compose content and replace the token/key values:

```yaml
services:
  ledger-bot:
    image: ghcr.io/dayou0168/telegram-ledger-bot:latest
    container_name: ledger-bot
    restart: unless-stopped
    environment:
      TELEGRAM_BOT_TOKEN: "replace-with-your-token"
      TELEGRAM_BOT_USERNAME: "replace-with-your-bot-username"
      TELEGRAM_API_BASE: "https://api.telegram.org"
      BOT_DB_PATH: "data/ledger_bot.db"
      BOT_TIMEZONE: "Asia/Shanghai"
      TRONGRID_API_BASE: "https://api.trongrid.io"
      TRONGRID_API_KEY: "replace-with-your-trongrid-key"
      TRON_USDT_CONTRACT: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
      TRON_POLL_INTERVAL_SECONDS: "5"
      TRON_INITIAL_LOOKBACK_MINUTES: "15"
      P2P_RATE_API_BASE: "https://p2p.army/api/fapi"
      P2P_RATE_FRONT_API: "NextVOF2Ozuh36mW0TCv"
      P2P_RATE_MARKET: "okx"
      P2P_RATE_FIAT_UNIT: "CNY"
      P2P_RATE_ASSET: "USDT"
      P2P_RATE_TRADE_METHODS: "aliPay"
    volumes:
      - ledger_bot_data:/app/data

volumes:
  ledger_bot_data:
```

Docker will create the persistent volume automatically. The SQLite database stays in `ledger_bot_data`.

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

TRONGRID_API_BASE=https://api.trongrid.io
TRONGRID_API_KEY=your-trongrid-key
TRON_USDT_CONTRACT=TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t
TRON_POLL_INTERVAL_SECONDS=5
TRON_INITIAL_LOOKBACK_MINUTES=15
P2P_RATE_API_BASE=https://p2p.army/api/fapi
P2P_RATE_FRONT_API=NextVOF2Ozuh36mW0TCv
P2P_RATE_MARKET=okx
P2P_RATE_FIAT_UNIT=CNY
P2P_RATE_ASSET=USDT
P2P_RATE_TRADE_METHODS=aliPay
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

This bot does not need a website port mapping. It uses Telegram long polling and outbound HTTP requests to Telegram/TronGrid.

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

Private chat tests:

```text
/start
地址监听
添加监听地址 TGhAAySHUUcEGua33pZZ88wP3bA6XSeQuZ 监控地址
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
