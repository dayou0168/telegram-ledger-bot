# Go Ledger Bot v2.4.9 Permissions

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
- Disable removes affected operators from active broadcast targets and stores an exact permission snapshot. Re-enable restores snapshot targets that still exist; explicit revocations made before a later disable are not resurrected.
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
- A primary may grant scopes to another active primary or to one of its own active secondaries. It cannot target another primary's secondary.
- A primary may grant a group when it owns that group or has an explicit group-use permission. The recipient receives use only; ownership and group-management rights do not move.
- A primary may grant a chat only when it has that exact direct-chat permission. A chat reachable only through a group is not delegable as a standalone chat.
- A primary may revoke only permission rows whose `granted_by` is that primary. Host/default may revoke any permission row.
- A secondary cannot grant or revoke broadcast scopes.
- `broadcast_operator_permissions.user_id` always references a `global_operators.user_id`.

## Broadcast Group Ownership

- The host can view and manage every broadcast group. Default operators retain global broadcast use and unrestricted target assignment, but do not gain group ownership management.
- An active primary may create a group. The new row stores that primary in `broadcast_groups.owner_user_id`.
- A primary may rename, delete, add members to, or remove members from only a group it owns. Explicit group-use permission never grants these operations.
- A primary may add only chats for which it has an exact direct-chat permission. Creating a group cannot expand its chat visibility or broadcast scope.
- A primary sees its direct chats, groups it owns, groups explicitly assigned for use, its own secondaries, and permission rows it granted or received. It does not see peer-only chats, peer-only groups, another primary's secondaries, or unrelated third-party grants.
- Secondary operators cannot create or manage groups.
- Bot group selectors require ownership or explicit group-use permission. Membership overlap alone does not reveal a group, and targets are rechecked against current PostgreSQL permissions immediately before sending.
- Only the host may transfer group management ownership. The target must be an active primary; default operators, other primaries, secondaries, disabled identities, and ordinary users cannot perform or receive an invalid transfer.
- A transfer never removes or rewrites existing chat/group use permissions. If the target primary lacks direct permissions for member chats, the host must either reject the whole transaction with the exact missing count or explicitly fill every missing chat permission in the same transaction. Auto-filled rows record the host in `granted_by`.

Group ownership uses a foreign key to `global_operators` and a trigger that accepts only a primary identity as a non-null owner. Group creation additionally locks and requires that owner to be active. Disabling a primary pauses its own management/use entry but preserves ownership so the host can still manage the group and re-enable restores the same owner. `NULL` means environment/host-managed or unresolved historical ownership; it is never interpreted as ownership by an arbitrary primary.

The transfer transaction locks the group and target operator, compares the submitted expected owner with the current row, validates all member-chat scopes, writes any requested direct grants and permission audits, then updates `owner_user_id`. Stale or concurrent submissions fail before mutation. It preserves `created_by` as historical evidence. Each successful owner change appends a row to immutable `broadcast_group_owner_transfer_events` with actor, previous owner, new owner, auto-filled count, and timestamp, plus an `owner_transferred` group audit event.

## Address Watch And Backend

Ordinary private users may manage up to `ADDRESS_WATCH_FREE_LIMIT` active addresses. Host/default and active primary/secondary global operators are unlimited. Single-group operators use the ordinary-user limit unless they also hold a global identity.

The host can see all backend address watches. Default/global operators see only their own watches. Ordinary users cannot enter the backend.

Host-only backend modules remain host-only. Default operators can manage unrestricted broadcast permission assignments. A primary can access its direct-chat list, owned/use-authorized groups, owned-group management, its own secondary-management, and grants made by that primary. Address-watch visibility is unchanged: only the host sees all owners; every other backend identity sees only its own addresses.

## Disable, Cache, Migration, And Audit

Global operator checks for invite, backend, ledger, broadcast entry, and unlimited address watches read current PostgreSQL state. They do not rely on the 10-second operator cache. Global changes also clear same-process permission state immediately.

The 10-second `BOT_OPERATOR_CACHE_TTL_SECONDS` boundary remains only for ordinary single-group operator checks across multiple bot processes. Same-process add/remove actively invalidates it. Undo bypasses it entirely.

The legacy active-`broadcast_operators` backfill is a strictly one-time migration and is skipped when upgrading a database that already had `global_operators`. Repeated startup cannot recreate a removed identity. The v2.4.3 hierarchy repair first quarantines legacy-derived identities, then uses host/parent/creator/audit evidence to normalize direct host grants to primary and their children to secondary. Ambiguous or explicitly disabled rows remain disabled. Environment identity shadows are detached. Broadcast scopes are preserved for identities recovered as active; scopes for disabled identities remain in `broadcast_operator_permission_snapshots` until that identity is re-enabled.

The v2.4.4 broadcast-group migration captures historical owner evidence exactly once. `created_by` is accepted as owner only when it identifies a current active primary, does not conflict with legacy disabled state or creation audit, and every existing group member is within that primary's direct-chat scope. Host/default creators remain environment-managed. Secondary, disabled, unknown, zero, conflicting, or out-of-scope creators remain `owner_user_id=NULL` and are recorded in `broadcast_group_owner_repair_candidates` for host review. The normalization marker makes repeated startup idempotent and never overwrites an already assigned or manually repaired owner.

Manual resolution of an ambiguous candidate must be a host-controlled transaction: lock the group and candidate, revalidate the proposed owner as an active primary, verify every member chat is in that primary's direct scope, set `owner_user_id`, mark the candidate `manual_primary_owner`, and insert a `broadcast_group_audit_events` row with the host actor and timestamp. Keeping the group host-managed uses `manual_environment_owner`. Deleting or renaming a group updates the candidate record in the same transaction so repair evidence never points at a stale live name.

`permission_audit_events` is append-only and records global-operator create/update/level/parent/re-enable/disable actions and broadcast grant/revoke/restore actions with actor, subject, scope, and timestamp. `broadcast_group_audit_events` is also append-only and records create, ownership normalization, transfer, rename, deletion, and member changes. `broadcast_group_owner_transfer_events` is append-only and keeps the ownership-specific before/after evidence and auto-fill count.

## Module Boundary

`ledger-chain-watcher` does not own Telegram user permissions. Ledger core, bot invite handling, private menus, broadcast, address watch, and admin web must consume the centralized policy and PostgreSQL capability layer. New capabilities must be added there before business modules use them.
