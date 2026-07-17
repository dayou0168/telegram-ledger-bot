# v2.4.11 Production Rollout

v2.4.11 changes only the Telegram bot broadcast replacement behavior. Do not restart or replace `ledger-chain-watcher` for this release.

## Preconditions

1. Confirm the v2.4.11 GitHub Release, `SHA256SUMS`, and bot image digest exist.
2. Record each bot container's current image and inspect the current Compose file.
3. Back up each bot PostgreSQL database and the two Compose files.
4. Keep the watcher running and verify its existing `/healthz` state before bot deployment.

## Deployment order

Upgrade one bot at a time:

1. Change only the bot image to `ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.11`.
2. Pull the exact image and recreate only that bot container.
3. Verify container health, restart count, Telegram `getMe`, admin login redirect, and recent logs.
4. Test one single-chat image replacement with no fixed text and confirm the original caption remains.
5. Only after the first bot passes, repeat for the next bot.

Do not run database migrations manually. The application starts against the existing schema and this release adds no migration.

## Rollback

Restore the recorded previous bot image in the affected Compose file and recreate only that bot container. No database rollback and no watcher rollback are required.
