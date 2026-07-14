# v2.4.7 Production Rollout

v2.4.7 is an urgent convergence fix for v2.4.6 and changes both the watcher and bot readiness behavior. Upgrade the watcher first and the bot immediately after realtime source readiness is confirmed.

## Before The Window

- Download all eight Release assets and verify `SHA256SUMS` plus each sidecar checksum.
- Record immutable bot and watcher digests from `image-digests.txt`; do not deploy by `latest` alone.
- Back up every bot database and the separate watcher database, then verify each dump with `pg_restore -l`.
- Preserve the current watcher binary, environment file, systemd unit, bot Compose file, and container inspect output.
- Record current open/leased gap counts, each task's `next_page`, fallback state, and watcher readiness fields for comparison.

The helper [deploy/production-rollout.sh](../deploy/production-rollout.sh) remains read-only by default. State-changing actions require `--apply`.

## Upgrade

1. Run `deploy/production-rollout.sh preflight`.
2. Install the verified v2.4.7 watcher binary or exact watcher image and restart only the watcher.
3. Require `/healthz` success and authenticated `/status` with `source_ready=true` and a boolean `continuity_ready` field.
4. Do not wait indefinitely for `/readyz` when only historical gaps remain. `continuity_ready=false` is expected until those gaps converge.
5. Pull the bot image by its immutable v2.4.7 digest and recreate every bot container.
6. Verify bot-to-watcher status access and confirm historical continuity alone does not activate public fallback.

## Migration Acceptance

Verify in both databases:

- `2.4.6-chain-gap-scheduler` and `2.4.7-chain-gap-convergence` exist exactly once;
- `chain_watcher_gap_tasks` contains `head_event_id` and `retry_after`;
- `idx_chain_watcher_gap_fair_claim`, ready-claim, overlap, and claim indexes exist;
- `chain_watcher_gap_metric_minutes` remains available.

Never truncate or rewrite active gap tasks during deployment.

## Runtime Acceptance

- Repeated status samples show progressed tasks retain or increase `next_page`; they do not rewind after merge or normalize.
- Fair claims rotate to other eligible windows instead of repeatedly reclaiming one old large task.
- Token-aware deferrals do not inflate gap failure counters when no API call occurred.
- `source_ready=true` remains stable; `continuity_ready` eventually becomes true as open and leased gaps converge.
- Bot logs show no public fallback activation caused only by `degraded/continuity`.
- No sustained panic, fatal, migration, decrypt, 401, 403, 429, deadline, chain outbox, or Telegram gateway errors occur.
- Complete one incoming and one outgoing USDT transfer test and record watcher match, bot claim, outbox send, and Telegram receive timing.

Observe gap convergence and readiness for at least 10 to 15 minutes. Production gap shape and real upstream throttling were not reproduced by local gates.

## Rollback Boundary

Runtime files may be restored from the verified backup only if the previous binaries remain forward-compatible. PostgreSQL migrations and the fair-claim index are forward-only. Do not remove migration rows or bulk-rewrite gap progress during rollback.
