# v2.4.4 Production Rollout

This release requires a bot database backup and bot container update. The versioned watcher image and host binary are republished for release coherence, but watcher logic is unchanged from v2.4.3 and a watcher restart is not required.

## Preconditions

- Use only the immutable bot digest recorded in the v2.4.4 Release.
- Back up each bot PostgreSQL database and verify the dump before replacing the container.
- Keep the existing bot environment, watcher URL/secret, PostgreSQL credentials, and stable instance ID unchanged.
- Do not apply bulk ownership SQL. Historical groups without conclusive primary-owner evidence must remain host-managed and appear in the repair-candidate table.

## Upgrade

1. Pull the v2.4.4 bot image by exact tag and record its repository digest.
2. Recreate the bot container without changing its database or environment.
3. Wait for migrations and Telegram polling to start successfully.
4. Confirm the container uses the expected immutable digest and existing log limits.

## Database acceptance

The bot database must contain:

- migration `2.4.4-broadcast-group-ownership` exactly once;
- column `broadcast_groups.owner_user_id`;
- tables `broadcast_group_owner_repair_candidates` and `broadcast_group_audit_events`;
- index `idx_broadcast_groups_owner`;
- no automatically assigned owner for ambiguous, disabled, secondary, unknown, or out-of-scope historical creators.

## Functional acceptance

- Host can view and manage every broadcast group.
- A primary can create and manage only its owned groups and can use explicitly assigned groups without gaining ownership.
- A secondary cannot create or manage groups.
- Group membership and send targets are rechecked against current permissions.
- The admin cleanup modal opens on desktop/mobile, saves hour/minute values, and the mobile operator search filters locally.
- Logs contain no panic, migration, template, storage, or Telegram polling errors.

PostgreSQL migrations are forward-only. Rolling the container back does not downgrade the schema. Production deployment is performed by the deployment task, not by the release workflow.
