# v2.4.14 Production Rollout

v2.4.14 changes the Telegram bot and its PostgreSQL schema. It does not change the watcher protocol, so existing production watcher processes should remain running.

This document is a deployment guide only. Publishing v2.4.14 must not modify production.

## Preconditions

1. Confirm the v2.4.14 tag, GitHub Release, `SHA256SUMS`, and bot image digest all point to the same release commit.
2. Back up each bot PostgreSQL database and record the current v2.4.13 bot image and digest.
3. Verify the existing watcher health, but do not replace or restart it for this release.
4. Inspect pending broadcast and quick-reply outbox rows before recreating a bot.
5. Do not import production UIDs, group IDs, observer grants, or hand-written fixtures during the upgrade.

## Migration risk

- `2.4.20-broadcast-message-preferences` adds independent send/reply preference fields to the existing durable preference table.
- Startup applies the migration in the existing advisory-locked transaction. Do not insert the marker or run its DDL manually.
- Observer grants remain the authorization ceiling. Existing preference rows are normalized so the old reply behavior is retained while send preference defaults remain compatible.
- Rollback to v2.4.13 leaves the added columns in place; do not drop them during an application rollback.

## Deployment order

Upgrade one bot at a time:

1. Set only the bot image to `ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.14` or the verified immutable digest from `image-digests.txt`.
2. Pull the exact image and recreate only that bot container. Do not recreate PostgreSQL or the watcher.
3. Wait for startup migration completion and confirm marker `2.4.20-broadcast-message-preferences` exists exactly once.
4. Verify container health, restart count, admin access, outbox processing, and recent logs.
5. Complete acceptance on the first bot before continuing to another instance.

## Acceptance

- Send text, photo, and photo-with-caption through `chat`, `group`, and `all`; verify upstream copies identify the sender and effective target.
- Reply from a target group and confirm recipients receive the original context and real media.
- Confirm no “发送中” placeholder appears and exactly one final receipt is sent.
- Exercise the single-chat replacement matrix, including text-only source to fixed image.
- Toggle send and reply preferences independently and verify no preference exceeds the observer grant ceiling.
- Revoke or change permission after entering quick reply but before send; the fresh check must block an ineligible delivery.

## Rollback

Stop the affected bot and inspect pending/retrying broadcast and quick-reply rows before rollback. Restore the recorded v2.4.13 bot image and recreate only that bot container. Keep PostgreSQL and the watcher running.

Do not drop the new columns or migration marker during a routine rollback. Restore the database backup only for a confirmed data-level incident while the affected bot is stopped.
