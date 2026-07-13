# v2.4.5 Production Rollout

This release requires a verified bot database backup and bot container update. The versioned watcher image and host package may be republished by the existing release workflow, but watcher code is unchanged and production does not need to update or restart it for v2.4.5.

## Preconditions

- Use only the immutable bot digest recorded in the v2.4.5 Release.
- Back up each bot PostgreSQL database and verify the dump before replacing the container.
- Keep the existing bot environment, watcher connection, PostgreSQL credentials, and stable instance ID unchanged.
- Do not run manual bulk owner updates and do not embed production owner repairs in fixtures.
- Record the current `broadcast_groups.owner_user_id` and unresolved repair-candidate counts for rollback diagnosis without exporting user identities into the repository.

## Upgrade

1. Pull the v2.4.5 bot image by exact tag and record its repository digest.
2. Recreate only the bot container without changing its database or environment.
3. Wait for the forward migration and Telegram polling to start successfully.
4. Confirm the container uses the expected immutable digest and existing log limits.

## Database acceptance

The bot database must contain:

- migrations `2.4.4-broadcast-group-ownership` and `2.4.5-broadcast-group-owner-transfer` exactly once each;
- column `broadcast_groups.owner_user_id`;
- existing tables `broadcast_group_owner_repair_candidates` and `broadcast_group_audit_events`;
- table `broadcast_group_owner_transfer_events`;
- indexes `idx_broadcast_group_owner_transfer_group`, `idx_broadcast_group_owner_transfer_actor`, and `idx_broadcast_group_owner_transfer_new_owner`;
- immutable trigger `trg_broadcast_group_owner_transfer_immutable`.

## Functional acceptance

- Host can transfer a group to an active primary and the “创建者” column immediately shows the new manager.
- With permission completion disabled, a target missing direct chat permissions receives an exact conflict and no owner or permission row changes.
- With completion enabled, all missing direct chat permissions, permission audits, owner update, and transfer audits commit together.
- A stale expected owner returns conflict and preserves the submitted form state for review.
- Primary and secondary administrators cannot see the transfer controls; direct non-host requests remain forbidden.
- Create, rename, transfer, membership, and delete controls remain usable on desktop and mobile.
- Logs contain no panic, migration, template, storage, permission, or Telegram polling errors.

PostgreSQL migrations and completed transfers are forward-only. Rolling back the runtime does not undo owner changes or remove the new audit objects. Production deployment is performed by the deployment task, not by the release workflow.
