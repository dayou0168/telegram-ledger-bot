# v2.4.12 Production Rollout

v2.4.12 changes the Telegram bot and its PostgreSQL schema. It does not change the watcher protocol, so existing production watcher processes should remain running.

## Preconditions

1. Confirm the v2.4.12 tag, GitHub Release, `SHA256SUMS`, and bot image digest all point to the same release commit.
2. Back up each bot PostgreSQL database and record the current bot image reference and digest.
3. Verify the existing watcher `/healthz` and `/readyz` state, but do not restart it for this release.
4. Do not import production UIDs, group IDs, or hand-written permission fixtures during the upgrade.

## Deployment order

Upgrade one bot at a time:

1. Set only the bot image to `ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.12` or the verified digest from `image-digests.txt`.
2. Pull the exact image and recreate only that bot container.
3. Wait for startup migrations, then verify container health, restart count, admin login, and recent logs.
4. Confirm the migration markers `2.4.16-chat-member-discovery` and `2.4.17-broadcast-reply-preferences` exist in `schema_migrations`.
5. Confirm `users.has_spoken`, `idx_users_chat_spoken_seen`, `broadcast_reply_preferences`, and its source-user lookup index exist.
6. Complete acceptance on the first bot before continuing to the next instance.

Do not run the migrations manually. Application startup applies them under the existing PostgreSQL advisory lock and transaction.

## Acceptance

- Reply to a user without a username and add/remove that user as a group operator.
- Select a no-username user through Telegram's clickable-name mention and confirm the same behavior.
- Confirm a plain typed nickname is rejected and duplicate reply/mention/username targets are applied only once.
- Confirm `chat_member` identity updates do not add silent members to mention-all; a real message makes the member eligible.
- Open the admin “回复通知” tab on desktop and mobile.
- Confirm the broadcast sender cannot be disabled, a primary sees only direct active secondaries, and an unauthorized POST returns 403.
- Send a broadcast reply and confirm only the sender plus enabled authorized recipients receive it.

## Rollback

Restore the recorded v2.4.11 bot image and recreate only the affected bot container. Keep the watcher running.

The migrations are forward-compatible with the previous binary. Do not drop the new column, table, indexes, or migration markers during a routine application rollback. Restore the database backup only for a confirmed data-level incident and only after stopping the affected bot.
