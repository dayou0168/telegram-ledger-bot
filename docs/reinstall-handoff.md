# Reinstall Handoff and Current State

Last updated: 2026-07-18

This document is the recovery handoff for reinstalling the local system and continuing work from the current GitHub state. It intentionally does not contain any real token, password, API key, or SSH credential.

## Current Project State

- Repository: `dayou0168/telegram-ledger-bot`
- Main line: Go + PostgreSQL
- Deprecated line: the old Python runtime is retired. Do not restore, test, or publish it as the active product line.
- Current source release candidate: `v2.4.12`
- Last confirmed published release before this candidate: `v2.4.11`
- The v2.4.12 release commit and URL do not exist until the explicit release workflow succeeds. Do not invent them from a local commit.

Intended v2.4.12 images after the explicit release workflow succeeds:

```text
ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.12
ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.4.12
```

## Current Architecture

- One bot container per bot instance.
- Each bot keeps its own PostgreSQL database for ledger, broadcast, forwarding, address-watch settings, notifications, and admin data.
- Shared `ledger-chain-watcher` is a separate service with its own PostgreSQL database.
- In normal mode, the bot registers watched addresses with `ledger-chain-watcher` and claims watcher matched events.
- Bot fallback is not the normal path. After sustained watcher failure, all bots compete for a PostgreSQL lease and only one shared no-key leader scans until the watcher has recovered and its watermark is caught up.
- Shared no-key fallback requires the watcher PostgreSQL DSN and a unique stable `BOT_FALLBACK_INSTANCE_ID` per bot; there is no per-bot emergency scanner switch or fixed maximum active time.
- v2.4.11 is the last confirmed GitHub release. The unpublished v2.4.12 candidate adds member discovery and per-user broadcast reply preferences through migrations `2.4.16-chat-member-discovery` and `2.4.17-broadcast-reply-preferences`; it does not change the watcher protocol. Use `docs/production-rollout-v2.4.12.md` only after the real Release exists.
- Ignore deployment files from the outer legacy worktree. The only candidate baseline is the confirmed integration-repository commit.

## v2.4.12 Candidate Runtime Configuration

Expected watcher-side values:

```env
CHAIN_WATCHER_SOURCE_POLL_SECONDS=1
CHAIN_WATCHER_MAIN_SCAN_TIMEOUT_MS=3000
CHAIN_WATCHER_MAIN_MAX_INFLIGHT_ROUNDS=3
CHAIN_WATCHER_HEAD_TIME_BUDGET_MS=850
CHAIN_WATCHER_HEAD_SAFETY_MAX_PAGES=256
CHAIN_WATCHER_HEAD_MAX_CONCURRENCY=32
CHAIN_WATCHER_HEAD_PERSIST_CONCURRENCY=8
CHAIN_WATCHER_RECOVERY_SAFETY_MAX_PAGES=4096
CHAIN_WATCHER_SURPLUS_BURST_SECONDS=60
CHAIN_WATCHER_CATCHUP_ENABLED=true
CHAIN_WATCHER_CATCHUP_STATE_INTERVAL_SECONDS=30
CHAIN_WATCHER_CATCHUP_PAGE_LIMIT=3
CHAIN_WATCHER_CATCHUP_MAX_REQUESTS_PER_TICK=6
CHAIN_WATCHER_CATCHUP_MAX_INFLIGHT=8
CHAIN_WATCHER_CATCHUP_MAX_RPS=0
CHAIN_WATCHER_TRONSCAN_DAILY_LIMIT_PER_KEY=100000
CHAIN_WATCHER_CATCHUP_WINDOW_SECONDS=30
CHAIN_WATCHER_CATCHUP_OVERLAP_SECONDS=2
CHAIN_WATCHER_LOOKBACK_SECONDS=600
CHAIN_WATCHER_TRONSCAN_API_KEYS=key1,key2,key3
CHAIN_WATCHER_TRONSCAN_KEY_INTERVAL_MS=200
CHAIN_WATCHER_KEY_ENCRYPTION_KEY=base64_encoded_32_byte_key
```

The main scan derives its page count from fractional sustainable base tokens; there is no fixed base-page count or key-count ceiling. A full healthy key contributes about one base page per second (86,400/day) and about 13,600/day to shared surplus. The local 100,000 count is planning and observability data; upstream 429 responses remain authoritative. API page concurrency is capped separately at 32. P1 has a reserved persistence lane while P2..PN use a bounded pool of eight workers. Successful pages commit independently; exact failed pages and missing anchors become fenced PostgreSQL gap tasks. Registry, batched usage, cooldown, cursor, and gap state persist across restarts; `/status` requires the admin Bearer token and exposes fingerprints only.

The supported in-flight variable is `CHAIN_WATCHER_MAIN_MAX_INFLIGHT_ROUNDS`. Do not use the unsupported name `CHAIN_WATCHER_MAIN_SCAN_MAX_INFLIGHT`.

Expected bot-side fallback values:

```env
BOT_WATCHER_HEALTH_INTERVAL_SECONDS=1
BOT_WATCHER_FAIL_THRESHOLD=3
BOT_FALLBACK_SHARED_DATABASE_URL=postgres://chainwatcher:***@chain-postgres:5432/ledger_chain_watcher?sslmode=disable
BOT_FALLBACK_INSTANCE_ID=unique-stable-id-for-this-bot
BOT_WATCHER_CLAIM_TIMEOUT_MS=2000
BOT_FALLBACK_POLL_SECONDS=1
```

Related templates:

- `.env.example`
- `docker-compose.ghcr.yml`
- `docker-compose.baota-host-pg.yml`
- `docker-compose.chain-watcher.yml`
- `deploy/ledger-chain-watcher.env.example`
- `deploy/ledger-chain-watcher.service`
- `docs/deployment.md`

## Historical Online Snapshot

Historical snapshot from the v2.3.1 release thread. It is not evidence that v2.4.1 has been deployed:

- Host `ledger-chain-watcher` was updated to `v2.3.1` from the GitHub Release linux-amd64 package.
- `zhuanfa-tianze-go` bot container was updated to `ghcr.io/dayou0168/telegram-ledger-bot-go:2.3.1`.
- `ledger-chain-watcher.service` was active.
- watcher `/healthz` returned OK.
- watcher `/readyz` should be used for chain-source readiness; `/status` shows latest source scan, backoff, pending, and cleanup state.
- bot container could access watcher `/healthz`.
- Short observation window showed no sustained `401`, `403`, `429`, or `ApiKey not exists` logs.
- watcher DB snapshot at that time:
  - active subscriptions: `2`
  - events: `0`
  - matched events: `0`
- bot DB snapshot at that time:
  - active address watches: `2`
  - chain notifications: `0`
  - pending notification outbox: `0`
- A real USDT transfer test is still required to confirm the end-to-end reminder loop.

## Thread and Collaboration Rules

- The long main thread is the coordinator only.
- Specific implementation should go to module threads.
- New module work should start from a blank thread, not by forking long history.
- Permission changes should go through the permission-system thread first.
- Releases, version bumps, README/Compose finalization, commits, pushes, Actions/GHCR checks, and server rollout should go through the total-control release thread.

Known module thread IDs:

```text
permissions:              019f3dfd-33c2-7680-89b1-b17513299102
broadcast/admin page:     019f3dfd-b01d-7ad3-ad04-5edd65445044
deployment operations:    019f3dfd-d56d-7fe1-85a4-d2c2d361dae6
total-control release:    019f3dfc-5b78-7711-9d6e-1495223902a7
chain watcher:            019f3dfd-835c-74d1-9e1f-4058d4544833
ledger core:              019f3dfd-5df8-7b13-ad43-63939658fcee
```

## Recovery After Reinstall

1. Install Git and configure GitHub authentication.

```powershell
git --version
gh auth login
gh auth status
```

2. Clone the repository.

```powershell
git clone https://github.com/dayou0168/telegram-ledger-bot.git
cd telegram-ledger-bot
git status --short --branch
```

3. Read the current docs before changing anything.

```text
README.md
go-ledger/README.md
docs/deployment.md
docs/reinstall-handoff.md
```

4. Prepare runtime infrastructure.

- Docker or BaoTa Docker Compose for bot containers.
- PostgreSQL for each bot database.
- Separate PostgreSQL database for shared `ledger-chain-watcher`.
- Optional host systemd watcher using `/usr/local/bin/ledger-chain-watcher` and `/etc/ledger-chain-watcher/env`.

5. Fill configuration from templates.

- Use placeholders in GitHub docs/templates only.
- Fill real values from a secure channel after reinstall.
- Keep each bot `DATABASE_URL` pointed at that bot's own database.
- Keep watcher `CHAIN_WATCHER_DATABASE_URL` pointed at the watcher database.
- Keep bot `CHAIN_WATCHER_BOT_ID` / `CHAIN_WATCHER_SECRET` aligned with watcher `CHAIN_WATCHER_BOTS`.

6. Pull and start the current images.

```powershell
docker pull ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.12
docker pull ghcr.io/dayou0168/telegram-ledger-chain-watcher:2.4.12
```

The v2.4.12 workflow publishes matching bot and watcher artifacts. For an existing v2.4.11 production installation, back up each bot database, verify the bot digest, and recreate each bot on v2.4.12; do not restart the watcher for this bot-only release.

7. Verify after startup.

Recommended checks:

```bash
systemctl is-active ledger-chain-watcher
curl -fsS http://127.0.0.1:8090/healthz
curl -fsS http://127.0.0.1:8090/readyz
curl -fsS -H "Authorization: Bearer $CHAIN_WATCHER_ADMIN_TOKEN" http://127.0.0.1:8090/status
docker exec zhuanfa-tianze-go wget -qO- http://host.docker.internal:8090/healthz
docker exec zhuanfa-tianze-go wget -qO- http://host.docker.internal:8090/readyz
journalctl -u ledger-chain-watcher --since "3 minutes ago" --no-pager
docker logs --since 3m zhuanfa-tianze-go
```

Database checks should confirm:

- watcher subscriptions exist for active address watches.
- watcher `events` and `matched_events` grow after a real chain event.
- bot `chain_notifications` and `notification_outbox` record notifications after a real match.

## Sensitive Information Policy

Never commit real secrets to GitHub. These values must be refilled from a secure channel after reinstall:

- Telegram bot token
- PostgreSQL passwords and full private DSNs
- SSH password or private keys
- Tronscan or TronGrid API keys
- Admin web token
- Internal `CHAIN_WATCHER_SECRET`
- Any backup archive password

Use examples such as `replace-me`, `change_this_password`, or `your_real_api_key` in repository files only.

## Next Priorities

1. Run a real USDT TRC20 transfer test and confirm watcher events, matched events, bot notification rows, and Telegram reminder delivery.
2. If reminders are still slow or unreliable, evaluate a non-public-API event source:
   - TRON Lite FullNode + Event Plugin V2 + Kafka
   - reliable webhook or stream provider
3. Verify every bot has the same shared fallback DSN; missing DSN must remain visibly degraded rather than starting a local scanner.
