# Go Ledger Bot v2.4.3 Permissions

This file is the source of truth for Telegram user permissions in the Go/PostgreSQL runtime. Business modules must use `internal/permissions` and the storage capability helpers instead of reading host/default configuration or legacy operator tables directly.

## Identities

- `BOT_HOST_USER_ID` is the only host and the only identity with complete backend administration.
- `DEFAULT_OPERATOR_USER_IDS` are maintenance-configured global privileged identities. They are not stored in PostgreSQL and cannot create or disable database global operators.
- Active `global_operators.level=primary` and `global_operators.level=secondary` are database global operators.
- Rows in `operators` are single-group ledger operators. They never grant invite, private backend, broadcast, global settings, or unlimited address-watch capability.
- `broadcast_operators` only carries backward-compatible private-cleanup settings. It is not a permission source.

## Global Operator Tree

- A primary has no parent.
- An active secondary must reference an active primary through `parent_user_id`.
- The host can create, update, re-enable, or disable primary operators. The host may also create or manage a secondary on behalf of a selected active primary; the secondary always stores that primary in `parent_user_id`.
- A primary can create, update, re-enable, or disable only its own secondary operators.
- A secondary cannot delegate.
- Disabling a primary also disables its active secondaries.
- Disable removes the affected operators' broadcast target permissions. Re-enable does not restore them.
- Host/default environment identities are never database global-operator authorization subjects and cannot receive target permissions.

Database checks, a self-reference foreign key, and a parent-validation trigger enforce these invariants independently of the web form.

## Ledger Permissions

The host, default operators, and active primary/secondary global operators can execute the complete ledger command set in a group where they issue the command. This includes start, stop, record, undo, rate and exchange-rate settings, and ledger clearing.

A single-group operator can execute ledger commands only in rows where `(chat_id, user_id)` exists in `operators`. Removing that row removes current ledger permission. Single-group ownership and operator management remain group-scoped and do not create global capability.

`+0` and the open-bill query remain readable by ordinary group members under the existing ledger query rules; this does not grant write permission.

## Undo Period Boundary

Undo requires both current ledger permission and a record in the current active ledger period:

- The record `day_key` must equal `currentLedgerDayKey(group, now)`.
- The group must still be active for that period.
- At the configured cutoff, the previous period is sealed for every identity, including host/default/global operators and the original recorder.
- Starting a new period never reopens a previous `day_key`.
- With cutoff disabled, the same active `active_day_key` remains one continuous period. Stopping or otherwise closing that period prevents undo until a new period is started, and prior `day_key` records remain sealed.
- Undo performs a direct current-permission database check rather than using the single-group operator TTL cache.

## Invite And Private Capabilities

The host, default operators, and active primary/secondary global operators can invite the bot. Single-group operators and ordinary users cannot.

The same global identities can use private global features. A disabled or invalid global operator cannot create a Telegram admin ticket and an existing signed operator session is rejected on its next request.

## Broadcast Delegation

- Host/default identities may grant or revoke broadcast group/chat scopes for any active database primary or secondary without a parent-scope restriction.
- A primary may grant or revoke scopes only for its own active secondaries.
- A primary may grant a group only when it has that exact group scope.
- A primary may grant a chat when it has that chat directly or through one of its group scopes.
- A secondary cannot grant or revoke broadcast scopes.
- `broadcast_operator_permissions.user_id` always references a `global_operators.user_id`.

## Address Watch And Backend

Ordinary private users may manage up to `ADDRESS_WATCH_FREE_LIMIT` active addresses. Host/default and active primary/secondary global operators are unlimited. Single-group operators use the ordinary-user limit unless they also hold a global identity.

The host can see all backend address watches. Default/global operators see only their own watches. Ordinary users cannot enter the backend.

Host-only backend modules remain host-only. Default operators can manage unrestricted broadcast permission assignments. A primary can access only its own secondary-management and delegated broadcast-permission views.

## Disable, Cache, Migration, And Audit

Global operator checks for invite, backend, ledger, broadcast entry, and unlimited address watches read current PostgreSQL state. They do not rely on the 10-second operator cache. Global changes also clear same-process permission state immediately.

The 10-second `BOT_OPERATOR_CACHE_TTL_SECONDS` boundary remains only for ordinary single-group operator checks across multiple bot processes. Same-process add/remove actively invalidates it. Undo bypasses it entirely.

The legacy active-`broadcast_operators` backfill is a strictly one-time migration and is skipped when upgrading a database that already had `global_operators`. Repeated startup cannot recreate a removed identity. The v2.4.3 hierarchy repair first quarantines legacy-derived identities, then uses host/parent/creator/audit evidence to normalize direct host grants to primary and their children to secondary. Ambiguous or explicitly disabled rows remain disabled. Environment identity shadows are detached, and no old target permissions are restored.

`permission_audit_events` is append-only and records global-operator create/update/level/parent/re-enable/disable actions and broadcast grant/revoke actions with actor, subject, scope, and timestamp.

## Module Boundary

`ledger-chain-watcher` does not own Telegram user permissions. Ledger core, bot invite handling, private menus, broadcast, address watch, and admin web must consume the centralized policy and PostgreSQL capability layer. New capabilities must be added there before business modules use them.
