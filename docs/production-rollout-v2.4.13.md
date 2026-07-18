# v2.4.13 Production Rollout

v2.4.13 changes the Telegram bot, its PostgreSQL schema, and the admin session contract. It does not change the watcher protocol, so existing production watcher processes should remain running.

This document is a deployment guide only. Publishing v2.4.13 must not modify production.

## Preconditions

1. Confirm the v2.4.13 tag, GitHub Release, `SHA256SUMS`, and bot image digest all point to the same release commit.
2. Back up each bot PostgreSQL database and record the current bot image reference and digest.
3. Generate a unique, strong `ADMIN_SESSION_SECRET` for every bot instance. Do not reuse the Bot Token, `ADMIN_WEB_TOKEN`, watcher secret, or a database password.
4. Decide whether `ADMIN_WEB_TOKEN` is needed as an optional second factor. Leaving it empty does not weaken the Telegram ticket identity requirement.
5. Verify the existing watcher `/healthz` and `/readyz` state, but do not replace or restart it for this release.
6. Do not import production UIDs, group IDs, observer grants, or hand-written permission fixtures during the upgrade.

If `ADMIN_SESSION_SECRET` is missing, the bot remains otherwise available but admin session creation and verification fail closed. Treat that as a failed deployment, not as a supported password-only fallback.

## Migration risk

- `2.4.18-operator-message-observers-admin-identity` creates observer grant/audit state and enforces active secondary-to-cross-primary relationships with database constraints and revocation behavior.
- `2.4.19-broadcast-delivery-state` creates durable target and upstream mapping tables and adds media/reply fields used by the notification outbox.
- Both migrations are forward-only. Startup applies them inside a transaction under the migration advisory lock; do not insert markers or run DDL manually.
- Databases that already have `2.4.18` but lack the broadcast tables must still run `2.4.19`. Verify the marker and physical tables, not the marker alone.
- A v2.4.12 binary ignores the new observer/target tables and does not understand v2.4.13 durable media delivery semantics. An application rollback can lose access to saved target/observer behavior and must not process an ambiguous backlog without inspection.

## Deployment order

Upgrade one bot at a time:

1. Add the bot-specific `ADMIN_SESSION_SECRET` through a protected env file or secret store; keep the literal value out of version-controlled Compose files, shell history, logs, and support messages.
2. Set only the bot image to `ghcr.io/dayou0168/telegram-ledger-bot-go:2.4.13` or the verified immutable digest from `image-digests.txt`.
3. Pull the exact image and recreate only that bot container. Do not recreate PostgreSQL or the watcher.
4. Wait for startup migrations, then verify health, restart count, admin login, and recent logs.
5. Confirm migration markers `2.4.18-operator-message-observers-admin-identity` and `2.4.19-broadcast-delivery-state` each exist exactly once.
6. Confirm `operator_message_observer_grants`, `operator_message_observer_audit_events`, `telegram_broadcast_targets`, and `broadcast_upstream_messages` exist and all indexes are valid/ready.
7. Complete acceptance on the first bot before continuing to another instance.

## Acceptance

- Log in from a fresh Telegram ticket as host, primary, and secondary; verify the current UID/role is enforced and a group-only operator is rejected.
- Disable a global operator and confirm its existing admin session stops authorizing requests.
- Confirm observer management is visible and writable only to the host; primary and secondary forged POST requests return 403.
- Send secondary text, photo, and photo-with-caption broadcasts and replies. Confirm the host, direct primary, and authorized observer primary follow their independent broadcast/reply switches with no duplicates.
- Restart the bot after selecting a broadcast target and confirm the target returns. Revoke permission and confirm the target is removed before another send.
- Rename and delete a broadcast group and confirm the durable target follows the rename and disappears on delete.
- Test both `关闭日切` and `设置日切-1`: active accounting continues across midnight/restart, while a stopped group remains stopped.
- Inspect retryable outbox rows, sent cleanup, Telegram reply mapping, and recent logs for repeated delivery or authorization errors.

## Rollback

Stop the affected bot before rollback and inspect pending/retrying notification outbox rows. If v2.4.13 media or upstream-copy rows are pending, drain or preserve them for a controlled forward recovery; do not let an older binary reinterpret an unknown backlog.

Restore the recorded v2.4.12 bot image and recreate only the affected bot container. Keep PostgreSQL and the watcher running. The old binary will not expose v2.4.13 observer or durable-target behavior.

Do not drop the new tables, columns, triggers, indexes, or migration markers during a routine application rollback. Restore the database backup only for a confirmed data-level incident and only while the affected bot is stopped.
