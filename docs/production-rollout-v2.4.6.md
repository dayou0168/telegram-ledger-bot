# v2.4.6 Production Rollout

This release changes both the shared watcher and the bot. Upgrade the watcher first, verify readiness, and only then update bots. Do not deploy v2.4.6 bot code against an older watcher during the acceptance window.

## Before The Window

- Download all eight Release assets and verify `SHA256SUMS` and each sidecar checksum.
- Record immutable bot and watcher digests from `image-digests.txt`; do not deploy by `latest` alone.
- Back up every bot PostgreSQL database and the separate watcher PostgreSQL database, then verify each dump can be listed by `pg_restore -l`.
- Preserve the existing watcher binary, environment file, systemd unit, bot Compose file, and container inspect output.
- Review the v2.4.6 environment examples. Remove retired fixed-page settings rather than carrying them forward.

The helper [deploy/production-rollout.sh](../deploy/production-rollout.sh) is read-only by default. State-changing actions require an explicit `--apply` argument.

## Upgrade

1. Run `deploy/production-rollout.sh preflight`.
2. Install the verified v2.4.6 watcher binary or exact watcher image.
3. Restart only the watcher and require `/healthz`, `/readyz`, and authenticated `/status` to succeed.
4. Confirm `/status` includes gap scheduler metrics, recent rounds, API wait/fetch timing, page counts, overlaps, and backoff state.
5. Pull the bot image by the immutable v2.4.6 digest and recreate bot containers.
6. Confirm bot-to-watcher health access before enabling normal traffic checks.

## Migration Acceptance

Verify in both databases:

- `schema_migrations.version='2.4.6-chain-gap-scheduler'` exists exactly once;
- `chain_watcher_gap_tasks` contains `head_event_id` and `retry_after`;
- `chain_watcher_gap_metric_minutes` exists;
- gap claim, overlap, and ready-claim indexes exist.

Pending or active gap tasks are operational state. Do not truncate them during deployment.

## Runtime Acceptance

- Watcher remains active and ready without restart loops.
- `/status` shows realtime rounds continuing while catch-up work is present.
- Sustained `401`, `403`, `429`, timeout, migration, decrypt, or scheduler errors are absent.
- Bot logs contain no panic, fatal, migration, chain outbox, or Telegram gateway failures.
- Priority-zero chain notifications reach the critical lane and do not wait behind bulk broadcasts.
- Run one real incoming and one real outgoing USDT transfer test and record watcher match, bot claim, outbox send, and Telegram receive timing.

Observe for 10 to 15 minutes before closing the window. Production load and real upstream throttling were not reproduced by local gates.

## Rollback Boundary

Runtime files may be rolled back from the verified backup if the old binaries remain forward-compatible. PostgreSQL migrations are forward-only and are not removed by runtime rollback. If compatibility is uncertain, restore both databases only with an explicit data-loss decision, otherwise redeploy v2.4.6 binaries.
