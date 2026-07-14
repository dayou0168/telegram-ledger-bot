# v2.4.9 Production Rollout

v2.4.9 changes both watcher request handling and bot fallback gap processing. It adds no migration or configuration, but watcher and bot must be upgraded together. Upgrade the watcher first, verify realtime source health, and then recreate every bot instance.

## Before The Window

- Download all eight Release assets and verify `SHA256SUMS` plus each sidecar checksum.
- Record immutable bot and watcher digests from `image-digests.txt`; do not deploy by `latest` alone.
- Preserve the current watcher binary, environment, systemd unit, bot Compose file, and container inspect output.
- Back up both databases and verify the dumps. No new migration is expected, but the backups protect the operational window.
- Record Key health/cooldowns, source and continuity readiness, fallback leader state, and representative gap task cursors.

The helper [deploy/production-rollout.sh](../deploy/production-rollout.sh) remains read-only by default. Use `upgrade --apply` for this release; `upgrade-watcher` alone is insufficient for v2.4.9.

## Upgrade

1. Run `deploy/production-rollout.sh preflight`.
2. Pin `NEW_WATCHER_BINARY`, `NEW_WATCHER_SHA256`, `NEW_BOT_IMAGE`, and `NEW_BOT_COMPOSE_FILE` to verified v2.4.9 artifacts.
3. Run `deploy/production-rollout.sh upgrade --apply`.
4. Require `/healthz` success and authenticated `/status` with `source_ready=true` before bot recreation proceeds.
5. Confirm every bot container runs the exact v2.4.9 digest and can reach watcher status.

## Migration Acceptance

- No v2.4.9 migration row should be created.
- Existing `2.4.6-chain-gap-scheduler` and `2.4.7-chain-gap-convergence` rows and indexes remain unchanged.
- Do not truncate, rewrite, or manually reset active gap tasks.

## Runtime Acceptance

- Parent scan-round cancellation does not add Key transport failures or cooldowns.
- Genuine independent request timeout, network error, and 5xx behavior still updates Key health as designed.
- Fallback `window` tasks continue paging to the configured safety limit when `end_page` is empty.
- Ordinary Advance/Yield never decreases `next_page`; only transactional Split creates children with an explicit reset cursor.
- `/healthz`, `/readyz`, authenticated `/status`, `source_ready`, and `continuity_ready` retain their existing contracts.
- No sustained panic, fatal, migration, decrypt, 401, 403, 429, deadline, fallback lease, chain outbox, or Telegram gateway errors occur.
- Complete one incoming and one outgoing USDT transfer test and record watcher match, bot claim, outbox send, and Telegram receive timing.

Observe request ownership, Key health, fallback progress, and cursor monotonicity for at least 10 to 15 minutes.

## Rollback Boundary

Restore the preserved v2.4.8 watcher and bot runtime files together. No database downgrade is required because v2.4.9 adds no migration. Do not rewrite gap cursor rows during rollback.
