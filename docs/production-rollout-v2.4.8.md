# v2.4.8 Production Rollout

v2.4.8 is a watcher-only hotfix for cancellation attribution. It adds no migration and does not change bot, readiness, or fallback contracts. Keep the production bot on v2.4.7 unless there is a separate operational reason to adopt the matching v2.4.8 image.

## Before The Window

- Download all eight Release assets and verify `SHA256SUMS` plus each sidecar checksum.
- Record the immutable watcher digest and binary checksum from `image-digests.txt` and `SHA256SUMS`; do not deploy by `latest` alone.
- Preserve the current watcher binary, environment file, systemd unit, and recent `/status` plus journal output.
- Back up the watcher database as an operational precaution. No v2.4.8 migration is expected.
- Record healthy Key counts, Key cooldown reasons, inflight rounds, source readiness, continuity readiness, and current gap counts.

The helper [deploy/production-rollout.sh](../deploy/production-rollout.sh) remains read-only by default. For this patch use `upgrade-watcher --apply`; the older `upgrade --apply` path also recreates the bot and is not required.

## Upgrade

1. Run `deploy/production-rollout.sh preflight`.
2. Set `NEW_WATCHER_BINARY` and `NEW_WATCHER_SHA256` from verified Release assets.
3. Run `deploy/production-rollout.sh upgrade-watcher --apply`.
4. Require `/healthz` success and authenticated `/status` with `source_ready=true` and a boolean `continuity_ready` field.
5. Confirm the bot container ID, image, and restart count did not change.

## Migration Acceptance

- No v2.4.8 migration row should be created.
- Existing `2.4.6-chain-gap-scheduler` and `2.4.7-chain-gap-convergence` rows and indexes remain unchanged.
- Never truncate, rewrite, or reset active gap tasks for this patch.

## Runtime Acceptance

- Concurrent rounds completing through a shared parent cancellation no longer record Key transport failure or add cooldown to a healthy Key.
- Genuine HTTP timeouts, network failures, and 5xx responses still affect Key health and cooldown as before.
- `/healthz`, `/readyz`, authenticated `/status`, `source_ready`, and `continuity_ready` retain v2.4.7 behavior.
- Bot logs show no restart and no change in fallback behavior caused by this rollout.
- No sustained panic, fatal, decrypt, 401, 403, 429, deadline, chain outbox, or Telegram gateway errors occur.
- Complete one incoming and one outgoing USDT transfer test and record watcher match, bot claim, outbox send, and Telegram receive timing.

Observe watcher concurrency and Key health for at least 10 to 15 minutes. Real Tronscan timeout and throttling behavior were not reproduced by local gates.

## Rollback Boundary

Restore the preserved v2.4.7 watcher binary and restart only the watcher. No database downgrade or bot rollback is required because v2.4.8 adds no migration and does not change the bot contract.
