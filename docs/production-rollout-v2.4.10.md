# v2.4.10 Production Rollout

v2.4.10 changes both the shared storage schema and the bot runtime. Upgrade the watcher first, verify realtime source health and migrations, and then recreate every bot instance. Do not deploy only one component.

## Before The Window

- Download all eight Release assets and verify `SHA256SUMS` plus each sidecar checksum.
- Record immutable bot and watcher digests from `image-digests.txt`; Compose examples use readable `2.4.10` tags, but production acceptance must compare immutable digests.
- Preserve the current watcher binary, environment, systemd unit, bot Compose file, and container inspect output.
- Back up both PostgreSQL databases and verify that both dumps can be read.
- Record watcher readiness, bot restart count, inbox/outbox depth, and representative pending Telegram work.

The helper [deploy/production-rollout.sh](../deploy/production-rollout.sh) remains read-only by default. Use `upgrade --apply`; `upgrade-watcher` alone is insufficient for v2.4.10.

## Upgrade

1. Run `deploy/production-rollout.sh preflight`.
2. Pin `NEW_WATCHER_BINARY`, `NEW_WATCHER_SHA256`, `NEW_BOT_IMAGE`, and `NEW_BOT_COMPOSE_FILE` to verified v2.4.10 artifacts.
3. Run `deploy/production-rollout.sh upgrade --apply`.
4. Require watcher `/healthz` success and authenticated `/status` with `source_ready=true` before bot recreation.
5. Confirm every bot container runs the exact v2.4.10 digest and can reach watcher status.

## Migration Acceptance

- Both databases contain exactly one `2.4.14-telegram-private-route-state` marker and one `2.4.15-telegram-quick-reply-outbox` marker after startup.
- `telegram_update_inbox`, `telegram_private_route_states`, and `telegram_quick_reply_outbox` exist.
- Inbox due/route/lease/cleanup indexes and quick-reply due/stream-actor/lease/cleanup indexes exist.
- Repeated startup is idempotent and does not duplicate markers or fail on existing objects.
- Never manually rewrite active inbox, route-state, or quick-reply outbox rows during rollout.

## Runtime Acceptance

- A process restart recovers inbox and quick-reply rows whose processing leases expire.
- A queued update uses current execution-time permissions and accounting period, not its original Telegram timestamp.
- A clear ticket is bound to its initiator and period, expires after 60 seconds, and is invalidated by cutoff changes.
- Revoking a global operator before delivery prevents that operator's queued quick reply from being sent.
- Telegram 429, 5xx, and network failures retry; 400 and 403 become terminal; terminal quick-reply rows age out after 72 hours.
- Critical ledger and chain notifications remain responsive while bulk broadcast traffic is active.
- `/healthz`, `/readyz`, authenticated `/status`, `source_ready`, and `continuity_ready` retain their existing contracts.
- No sustained panic, fatal, migration, storage, inbox, quick-reply, send-gateway, outbox, watcher, 401, 403, or 429 errors occur.

Observe queue depth, retries, lease recovery, bot restarts, and watcher readiness for at least 10 to 15 minutes. Complete representative ledger, clear-ticket, quick-reply, broadcast, and incoming/outgoing USDT tests.

## Rollback Boundary

The migrations are forward-only. Do not attempt a schema downgrade. If v2.4.10 must be rolled back, first stop new writes and assess whether the previous binaries are forward-compatible with the migrated schema. The safest recovery is to redeploy v2.4.10; restoring both pre-upgrade database backups is a separate data-loss decision that must be explicit.

Telegram can accept a request even when its HTTP acknowledgement is lost. This external ACK uncertainty remains after rollback and should be handled by audit and operator review rather than destructive queue edits.
