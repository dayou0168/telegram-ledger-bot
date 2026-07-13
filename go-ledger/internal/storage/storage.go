package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool      *pgxpool.Pool
	keyCipher *keyCipher
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	return open(ctx, databaseURL, true)
}

func OpenExisting(ctx context.Context, databaseURL string) (*Store, error) {
	return open(ctx, databaseURL, false)
}

func OpenChainWatcher(ctx context.Context, databaseURL, encryptionKey string) (*Store, error) {
	cipher, err := newKeyCipher(encryptionKey)
	if err != nil {
		return nil, err
	}
	store, err := open(ctx, databaseURL, false)
	if err != nil {
		return nil, err
	}
	store.keyCipher = cipher
	if err := store.migrate(ctx); err != nil {
		store.Close()
		return nil, err
	}
	if err := store.migrateTronscanKeyEncryption(ctx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func open(ctx context.Context, databaseURL string, migrate bool) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 32
	cfg.MinConns = 4
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	store := &Store{pool: pool}
	if migrate {
		if err := store.migrate(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS processed_updates (
			update_id BIGINT PRIMARY KEY,
			processed_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS groups (
			chat_id BIGINT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT '',
			active BOOLEAN NOT NULL DEFAULT FALSE,
			active_day_key TEXT NOT NULL DEFAULT '',
			active_expires_day_key TEXT NOT NULL DEFAULT '',
			active_period_started_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00',
			business_open BOOLEAN NOT NULL DEFAULT TRUE,
			owner_user_id BIGINT NOT NULL DEFAULT 0,
			deposit_rate TEXT NOT NULL DEFAULT '0',
			payout_rate TEXT NOT NULL DEFAULT '0',
			deposit_exchange_rate TEXT NOT NULL DEFAULT '1',
			payout_exchange_rate TEXT NOT NULL DEFAULT '1',
			exchange_rate_source TEXT NOT NULL DEFAULT '',
			exchange_rate_rank INTEGER NOT NULL DEFAULT 0,
			exchange_rate_offset TEXT NOT NULL DEFAULT '',
			fee_rate TEXT NOT NULL DEFAULT '0',
			cutoff_hour INTEGER NOT NULL DEFAULT 0,
			all_members_can_record BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_groups_updated_negative
			ON groups(updated_at DESC, chat_id)
			WHERE chat_id < 0`,
		`CREATE INDEX IF NOT EXISTS idx_groups_title_negative
			ON groups(title ASC, chat_id ASC)
			WHERE chat_id < 0`,
		`ALTER TABLE groups ADD COLUMN IF NOT EXISTS active_day_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN IF NOT EXISTS active_expires_day_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN IF NOT EXISTS active_period_started_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00'`,
		`WITH migration AS (
			INSERT INTO schema_migrations(version, applied_at)
			VALUES('2.4.2-active-period-start', NOW())
			ON CONFLICT(version) DO NOTHING
			RETURNING 1
		)
		UPDATE groups g
		SET active_period_started_at=CASE WHEN g.active THEN NOW() ELSE '1970-01-01 00:00:00+00'::timestamptz END
		FROM migration`,
		`ALTER TABLE groups ADD COLUMN IF NOT EXISTS exchange_rate_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN IF NOT EXISTS exchange_rate_rank INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE groups ADD COLUMN IF NOT EXISTS exchange_rate_offset TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS users (
			chat_id BIGINT NOT NULL,
			user_id BIGINT NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			last_seen_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(chat_id, user_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_users_chat_seen
			ON users(chat_id, last_seen_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_users_chat_username_lower
			ON users(chat_id, lower(username))
			WHERE username <> ''`,
		`CREATE TABLE IF NOT EXISTS operators (
			chat_id BIGINT NOT NULL,
			user_id BIGINT NOT NULL,
			role TEXT NOT NULL DEFAULT 'operator',
			added_by BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(chat_id, user_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_operators_chat_role
			ON operators(chat_id, role, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_operators_chat_order
			ON operators(chat_id, role, created_at, user_id)`,
		`CREATE TABLE IF NOT EXISTS broadcast_operators (
			user_id BIGINT PRIMARY KEY,
			status TEXT NOT NULL DEFAULT 'active',
			created_by BIGINT NOT NULL DEFAULT 0,
			remark TEXT NOT NULL DEFAULT '',
			private_cleanup_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			private_cleanup_time TEXT NOT NULL DEFAULT '',
			private_cleanup_last_run_date TEXT NOT NULL DEFAULT '',
			private_cleanup_bot_after_seconds INTEGER NOT NULL DEFAULT 0,
			private_cleanup_incoming_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			private_cleanup_incoming_after_seconds INTEGER NOT NULL DEFAULT 0,
			private_cleanup_scope TEXT NOT NULL DEFAULT 'broadcast,quick_reply,menu',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`ALTER TABLE broadcast_operators ADD COLUMN IF NOT EXISTS private_cleanup_enabled BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE broadcast_operators ADD COLUMN IF NOT EXISTS private_cleanup_time TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE broadcast_operators ADD COLUMN IF NOT EXISTS private_cleanup_last_run_date TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE broadcast_operators ADD COLUMN IF NOT EXISTS private_cleanup_bot_after_seconds INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE broadcast_operators ADD COLUMN IF NOT EXISTS private_cleanup_incoming_enabled BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE broadcast_operators ADD COLUMN IF NOT EXISTS private_cleanup_incoming_after_seconds INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE broadcast_operators ADD COLUMN IF NOT EXISTS private_cleanup_scope TEXT NOT NULL DEFAULT 'broadcast,quick_reply,menu'`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_operators_status
			ON broadcast_operators(status, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_operators_list
			ON broadcast_operators(status, updated_at DESC, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_operators_created_by
			ON broadcast_operators(created_by, status, created_at, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_operators_cleanup
			ON broadcast_operators(private_cleanup_enabled, private_cleanup_time, user_id)
			WHERE status='active'`,
		`INSERT INTO schema_migrations(version, applied_at)
			SELECT '2.4.2-global-operators-table-preexisting', NOW()
			WHERE to_regclass('global_operators') IS NOT NULL
			ON CONFLICT(version) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS global_operators (
			user_id BIGINT PRIMARY KEY,
			level TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			parent_user_id BIGINT,
			created_by BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			disabled_by BIGINT,
			disabled_at TIMESTAMPTZ,
			remark TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_global_operators_status
			ON global_operators(status, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_global_operators_level_status
			ON global_operators(level, status, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_global_operators_parent_status
			ON global_operators(parent_user_id, status, user_id)`,
		`WITH migration AS (
			INSERT INTO schema_migrations(version, applied_at)
			VALUES('2.4.2-global-operators-backfill-once', NOW())
			ON CONFLICT(version) DO NOTHING
			RETURNING 1
		)
		INSERT INTO global_operators(user_id, level, status, parent_user_id, created_by, created_at, remark)
			SELECT b.user_id,
				CASE WHEN b.created_by = 0 THEN 'primary' ELSE 'secondary' END,
				'active',
				NULLIF(b.created_by, 0),
				b.created_by,
				b.created_at,
				b.remark
			FROM broadcast_operators b
			CROSS JOIN migration
			WHERE b.status='active'
			  AND NOT EXISTS (
				SELECT 1 FROM schema_migrations
				WHERE version='2.4.2-global-operators-table-preexisting'
			  )
			ON CONFLICT(user_id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS permission_audit_events (
			id BIGSERIAL PRIMARY KEY,
			actor_user_id BIGINT NOT NULL DEFAULT 0,
			subject_type TEXT NOT NULL,
			subject_user_id BIGINT NOT NULL DEFAULT 0,
			action TEXT NOT NULL,
			level TEXT NOT NULL DEFAULT '',
			parent_user_id BIGINT,
			target_type TEXT NOT NULL DEFAULT '',
			chat_id BIGINT NOT NULL DEFAULT 0,
			group_name TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_permission_audit_subject
			ON permission_audit_events(subject_type, subject_user_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_permission_audit_actor
			ON permission_audit_events(actor_user_id, created_at DESC, id DESC)`,
		`CREATE OR REPLACE FUNCTION reject_permission_audit_mutation() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'permission audit events are immutable';
		END;
		$$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_permission_audit_immutable ON permission_audit_events`,
		`CREATE TRIGGER trg_permission_audit_immutable
			BEFORE UPDATE OR DELETE ON permission_audit_events
			FOR EACH ROW EXECUTE FUNCTION reject_permission_audit_mutation()`,
		`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM schema_migrations WHERE version='2.4.2-global-operators-safety'
			) THEN
				UPDATE global_operators
				SET level='primary', parent_user_id=NULL, status='disabled',
					disabled_at=COALESCE(disabled_at, NOW())
				WHERE level NOT IN ('primary', 'secondary');

				UPDATE global_operators
				SET status='disabled', disabled_at=COALESCE(disabled_at, NOW())
				WHERE status NOT IN ('active', 'disabled');

				UPDATE global_operators SET parent_user_id=NULL WHERE level='primary';

				UPDATE global_operators child
				SET level='primary', parent_user_id=NULL, status='disabled',
					disabled_at=COALESCE(child.disabled_at, NOW())
				WHERE child.level='secondary'
				  AND NOT EXISTS (
					SELECT 1 FROM global_operators parent
					WHERE parent.user_id=child.parent_user_id
					  AND parent.level='primary'
					  AND parent.status='active'
				  );

				UPDATE broadcast_operators b
				SET status='disabled', updated_at=NOW()
				WHERE EXISTS (
					SELECT 1 FROM global_operators g
					WHERE g.user_id=b.user_id AND g.status='disabled'
				);
				INSERT INTO schema_migrations(version, applied_at)
				VALUES('2.4.2-global-operators-safety', NOW());
			END IF;
		END $$`,
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='global_operators_level_check') THEN
				ALTER TABLE global_operators ADD CONSTRAINT global_operators_level_check
					CHECK (level IN ('primary', 'secondary'));
			END IF;
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='global_operators_status_check') THEN
				ALTER TABLE global_operators ADD CONSTRAINT global_operators_status_check
					CHECK (status IN ('active', 'disabled'));
			END IF;
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='global_operators_parent_shape_check') THEN
				ALTER TABLE global_operators ADD CONSTRAINT global_operators_parent_shape_check
					CHECK ((level='primary' AND parent_user_id IS NULL) OR (level='secondary' AND parent_user_id IS NOT NULL));
			END IF;
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='global_operators_parent_fkey') THEN
				ALTER TABLE global_operators ADD CONSTRAINT global_operators_parent_fkey
					FOREIGN KEY(parent_user_id) REFERENCES global_operators(user_id) ON DELETE RESTRICT;
			END IF;
		END $$`,
		`CREATE OR REPLACE FUNCTION validate_global_operator_parent() RETURNS trigger AS $$
		BEGIN
			IF NEW.level='primary' AND NEW.parent_user_id IS NOT NULL THEN
				RAISE EXCEPTION 'primary global operator cannot have a parent';
			END IF;
			IF NEW.level='secondary' AND NEW.status='active' AND NOT EXISTS (
				SELECT 1 FROM global_operators parent
				WHERE parent.user_id=NEW.parent_user_id
				  AND parent.level='primary'
				  AND parent.status='active'
			) THEN
				RAISE EXCEPTION 'active secondary requires an active primary parent';
			END IF;
			IF TG_OP='UPDATE' AND OLD.level='primary' AND OLD.status='active'
			   AND (NEW.level<>'primary' OR NEW.status<>'active') AND EXISTS (
				SELECT 1 FROM global_operators child
				WHERE child.parent_user_id=OLD.user_id
				  AND child.level='secondary'
				  AND child.status='active'
			) THEN
				RAISE EXCEPTION 'active primary still has active secondary operators';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS trg_validate_global_operator_parent ON global_operators`,
		`CREATE TRIGGER trg_validate_global_operator_parent
			BEFORE INSERT OR UPDATE OF level, status, parent_user_id ON global_operators
			FOR EACH ROW EXECUTE FUNCTION validate_global_operator_parent()`,
		`CREATE TABLE IF NOT EXISTS private_chat_messages (
			id BIGSERIAL PRIMARY KEY,
			operator_user_id BIGINT NOT NULL,
			chat_id BIGINT NOT NULL,
			message_id BIGINT NOT NULL,
			direction TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL DEFAULT '',
			cleanup_after_seconds INTEGER NOT NULL DEFAULT 0,
			due_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ,
			last_error TEXT NOT NULL DEFAULT '',
			UNIQUE(chat_id, message_id)
		)`,
		`ALTER TABLE private_chat_messages ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE private_chat_messages ADD COLUMN IF NOT EXISTS cleanup_after_seconds INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE private_chat_messages ADD COLUMN IF NOT EXISTS due_at TIMESTAMPTZ`,
		`CREATE INDEX IF NOT EXISTS idx_private_chat_messages_operator_pending
			ON private_chat_messages(operator_user_id, id)
			WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_private_chat_messages_due
			ON private_chat_messages(due_at, id)
			WHERE deleted_at IS NULL AND due_at IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_private_chat_messages_created
			ON private_chat_messages(created_at)`,
		`CREATE TABLE IF NOT EXISTS broadcast_groups (
			name TEXT PRIMARY KEY,
			created_by BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_groups_updated
			ON broadcast_groups(updated_at DESC, name)`,
		`CREATE TABLE IF NOT EXISTS broadcast_group_chats (
			group_name TEXT NOT NULL REFERENCES broadcast_groups(name) ON DELETE CASCADE,
			chat_id BIGINT NOT NULL REFERENCES groups(chat_id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(group_name, chat_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_group_chats_chat
			ON broadcast_group_chats(chat_id, group_name)`,
		`CREATE TABLE IF NOT EXISTS broadcast_operator_permissions (
			user_id BIGINT NOT NULL,
			target TEXT NOT NULL,
			chat_id BIGINT NOT NULL DEFAULT 0,
			group_name TEXT NOT NULL DEFAULT '',
			granted_by BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(user_id, target, chat_id, group_name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_permissions_user
			ON broadcast_operator_permissions(user_id, target, group_name, chat_id)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_permissions_chat
			ON broadcast_operator_permissions(user_id, target, chat_id)
			WHERE target = 'chat'`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_permissions_group
			ON broadcast_operator_permissions(user_id, target, group_name)
			WHERE target = 'group'`,
		`WITH migration AS (
			INSERT INTO schema_migrations(version, applied_at)
			VALUES('2.4.2-disabled-global-operator-permissions', NOW())
			ON CONFLICT(version) DO NOTHING
			RETURNING 1
		)
		DELETE FROM broadcast_operator_permissions p
		USING migration
		WHERE NOT EXISTS (
			SELECT 1 FROM global_operators g
			WHERE g.user_id=p.user_id AND g.status='active' AND g.level IN ('primary', 'secondary')
		)
		OR p.target NOT IN ('chat', 'group')
		OR (p.target='chat' AND (p.chat_id=0 OR p.group_name<>''))
		OR (p.target='group' AND (p.chat_id<>0 OR p.group_name=''))`,
		`DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='broadcast_permissions_target_check') THEN
				ALTER TABLE broadcast_operator_permissions ADD CONSTRAINT broadcast_permissions_target_check
					CHECK (target IN ('chat', 'group'));
			END IF;
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='broadcast_permissions_shape_check') THEN
				ALTER TABLE broadcast_operator_permissions ADD CONSTRAINT broadcast_permissions_shape_check
					CHECK ((target='chat' AND chat_id<>0 AND group_name='') OR
					       (target='group' AND chat_id=0 AND group_name<>''));
			END IF;
			IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='broadcast_permissions_user_fkey') THEN
				ALTER TABLE broadcast_operator_permissions ADD CONSTRAINT broadcast_permissions_user_fkey
					FOREIGN KEY(user_id) REFERENCES global_operators(user_id) ON DELETE CASCADE;
			END IF;
		END $$`,
		`CREATE TABLE IF NOT EXISTS admin_login_tickets (
			token_hash TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			role TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			used_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_login_tickets_user
			ON admin_login_tickets(user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_login_tickets_expires
			ON admin_login_tickets(expires_at)`,
		`CREATE TABLE IF NOT EXISTS broadcast_deliveries (
			id BIGSERIAL PRIMARY KEY,
			operator_user_id BIGINT NOT NULL,
			source_chat_id BIGINT NOT NULL,
			source_message_id BIGINT NOT NULL,
			target_chat_id BIGINT NOT NULL,
			target_title TEXT NOT NULL DEFAULT '',
			target_message_id BIGINT NOT NULL,
			mode TEXT NOT NULL DEFAULT '',
			target_name TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			replaced_at TIMESTAMPTZ
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_broadcast_deliveries_target_message
			ON broadcast_deliveries(target_chat_id, target_message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_deliveries_operator_created
			ON broadcast_deliveries(operator_user_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS broadcast_replace_settings (
			id INTEGER PRIMARY KEY DEFAULT 1,
			enabled BOOLEAN NOT NULL DEFAULT FALSE,
			text TEXT NOT NULL DEFAULT '',
			image_name TEXT NOT NULL DEFAULT '',
			image_data BYTEA,
			updated_at TIMESTAMPTZ NOT NULL,
			CHECK(id = 1)
		)`,
		`CREATE TABLE IF NOT EXISTS records (
			id BIGSERIAL PRIMARY KEY,
			chat_id BIGINT NOT NULL,
			day_key TEXT NOT NULL,
			kind TEXT NOT NULL,
			currency TEXT NOT NULL DEFAULT 'CNY',
			amount TEXT NOT NULL,
			rate TEXT NOT NULL,
			fee_rate TEXT NOT NULL,
			result_usdt TEXT NOT NULL,
			subject_user_id BIGINT NOT NULL DEFAULT 0,
			subject_name TEXT NOT NULL DEFAULT '',
			actor_user_id BIGINT NOT NULL,
			actor_name TEXT NOT NULL DEFAULT '',
			source_message_id BIGINT NOT NULL DEFAULT 0,
			bot_message_id BIGINT NOT NULL DEFAULT 0,
			remark TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			deleted_at TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_day_active
			ON records(chat_id, day_key, id)
			WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_day_created_active
			ON records(chat_id, day_key, created_at, id)
			WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_active_id
			ON records(chat_id, id DESC)
			WHERE deleted_at IS NULL`,
		`ALTER TABLE records ADD COLUMN IF NOT EXISTS subject_user_id BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE records ADD COLUMN IF NOT EXISTS subject_name TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_kind_active
			ON records(chat_id, kind, id DESC)
			WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_day_kind_active
			ON records(chat_id, day_key, kind, id DESC)
			WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_subject_day_active
			ON records(chat_id, subject_user_id, day_key, id)
			WHERE deleted_at IS NULL AND subject_user_id <> 0`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_source_message
			ON records(chat_id, source_message_id, id DESC)
			WHERE deleted_at IS NULL AND source_message_id <> 0`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_bot_message
			ON records(chat_id, bot_message_id, id DESC)
			WHERE deleted_at IS NULL AND bot_message_id <> 0`,
		`CREATE TABLE IF NOT EXISTS address_watches (
			owner_user_id BIGINT NOT NULL,
			address TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			watch_income BOOLEAN NOT NULL DEFAULT TRUE,
			watch_expense BOOLEAN NOT NULL DEFAULT TRUE,
			notify_trx BOOLEAN NOT NULL DEFAULT TRUE,
			min_notify_amount TEXT NOT NULL DEFAULT '0',
			active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(owner_user_id, address)
		)`,
		`ALTER TABLE address_watches ADD COLUMN IF NOT EXISTS watch_income BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE address_watches ADD COLUMN IF NOT EXISTS watch_expense BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE address_watches ADD COLUMN IF NOT EXISTS notify_trx BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE address_watches ADD COLUMN IF NOT EXISTS min_notify_amount TEXT NOT NULL DEFAULT '0'`,
		`CREATE INDEX IF NOT EXISTS idx_address_watches_active
			ON address_watches(active, owner_user_id, address)`,
		`CREATE INDEX IF NOT EXISTS idx_address_watches_owner_active
			ON address_watches(owner_user_id, updated_at DESC, address)
			WHERE active = TRUE`,
		`CREATE TABLE IF NOT EXISTS address_watch_settings (
			owner_user_id BIGINT PRIMARY KEY,
			watch_income BOOLEAN NOT NULL DEFAULT TRUE,
			watch_expense BOOLEAN NOT NULL DEFAULT TRUE,
			notify_trx BOOLEAN NOT NULL DEFAULT TRUE,
			min_notify_amount TEXT NOT NULL DEFAULT '0',
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS address_validations (
			chat_id BIGINT NOT NULL,
			address TEXT NOT NULL,
			verify_count INTEGER NOT NULL DEFAULT 0,
			first_user_id BIGINT NOT NULL DEFAULT 0,
			first_user_name TEXT NOT NULL DEFAULT '',
			last_user_id BIGINT NOT NULL DEFAULT 0,
			last_user_name TEXT NOT NULL DEFAULT '',
			last_seen_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(chat_id, address)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_address_validations_chat_seen
			ON address_validations(chat_id, last_seen_at DESC)`,
		`CREATE TABLE IF NOT EXISTS chain_notifications (
			owner_user_id BIGINT NOT NULL,
			address TEXT NOT NULL,
			tx_hash TEXT NOT NULL,
			direction TEXT NOT NULL,
			block_timestamp BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(owner_user_id, address, tx_hash, direction)
		)`,
		`ALTER TABLE chain_notifications ADD COLUMN IF NOT EXISTS event_id TEXT NOT NULL DEFAULT ''`,
		`UPDATE chain_notifications SET event_id=tx_hash WHERE event_id=''`,
		`ALTER TABLE chain_notifications DROP CONSTRAINT IF EXISTS chain_notifications_pkey`,
		`ALTER TABLE chain_notifications ADD PRIMARY KEY(owner_user_id, address, event_id, direction)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_notifications_latest
			ON chain_notifications(owner_user_id, address, block_timestamp DESC)`,
		`CREATE TABLE IF NOT EXISTS chain_watcher_bots (
			bot_id TEXT PRIMARY KEY,
			secret TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_bots_status
			ON chain_watcher_bots(status, bot_id)`,
		`CREATE TABLE IF NOT EXISTS chain_watcher_key_usage (
			fingerprint TEXT NOT NULL,
			budget_day DATE NOT NULL,
			request_count INTEGER NOT NULL DEFAULT 0,
			main_request_count INTEGER NOT NULL DEFAULT 0,
			comp_request_count INTEGER NOT NULL DEFAULT 0,
			other_request_count INTEGER NOT NULL DEFAULT 0,
			failover_count INTEGER NOT NULL DEFAULT 0,
			rate_limit_count INTEGER NOT NULL DEFAULT 0,
			auth_error_count INTEGER NOT NULL DEFAULT 0,
			last_http_status INTEGER NOT NULL DEFAULT 0,
			last_429_at TIMESTAMPTZ,
			cooldown_until TIMESTAMPTZ,
			disabled_until TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(fingerprint, budget_day)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_key_usage_day
			ON chain_watcher_key_usage(budget_day, fingerprint)`,
		`CREATE TABLE IF NOT EXISTS chain_watcher_api_keys (
			fingerprint TEXT PRIMARY KEY,
			api_key TEXT NOT NULL DEFAULT '',
			api_key_ciphertext BYTEA,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			health TEXT NOT NULL DEFAULT 'suspect',
			reason TEXT NOT NULL DEFAULT 'new_or_updated',
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			consecutive_auth_failures INTEGER NOT NULL DEFAULT 0,
			consecutive_probe_successes INTEGER NOT NULL DEFAULT 0,
			cooldown_until TIMESTAMPTZ,
			next_probe_at TIMESTAMPTZ,
			last_used_at TIMESTAMPTZ,
			last_success_at TIMESTAMPTZ,
			last_failure_at TIMESTAMPTZ,
			last_error_class TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`ALTER TABLE chain_watcher_api_keys ADD COLUMN IF NOT EXISTS api_key_ciphertext BYTEA`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_api_keys_enabled
			ON chain_watcher_api_keys(enabled, health, updated_at)`,
		`CREATE TABLE IF NOT EXISTS chain_watcher_runtime_state (
			id SMALLINT PRIMARY KEY DEFAULT 1 CHECK(id=1),
			global_watermark_timestamp BIGINT NOT NULL DEFAULT 0,
			global_watermark_tx_hash TEXT NOT NULL DEFAULT '',
			watermark_source TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS realtime_watermark_timestamp BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS realtime_watermark_tx_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS realtime_updated_at TIMESTAMPTZ`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS fallback_head_timestamp BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS fallback_anchor_event_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS fallback_head_updated_at TIMESTAMPTZ`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS catchup_required BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS catchup_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chain_watcher_runtime_state ADD COLUMN IF NOT EXISTS catchup_updated_at TIMESTAMPTZ`,
		`CREATE TABLE IF NOT EXISTS chain_watcher_fallback_lease (
			lease_name TEXT PRIMARY KEY,
			holder_id TEXT NOT NULL DEFAULT '',
			lease_until TIMESTAMPTZ NOT NULL,
			mode TEXT NOT NULL DEFAULT 'PRIMARY',
			started_at TIMESTAMPTZ,
			last_watcher_success TIMESTAMPTZ,
			fallback_requests BIGINT NOT NULL DEFAULT 0,
			fallback_429 BIGINT NOT NULL DEFAULT 0,
			catchup_from BIGINT NOT NULL DEFAULT 0,
			catchup_to BIGINT NOT NULL DEFAULT 0,
			catchup_pages BIGINT NOT NULL DEFAULT 0,
			catchup_budget_used BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_fallback_lease_expiry
			ON chain_watcher_fallback_lease(lease_until)`,
		`CREATE TABLE IF NOT EXISTS chain_watcher_subscriptions (
			bot_id TEXT NOT NULL,
			chat_id BIGINT NOT NULL DEFAULT 0,
			owner_user_id BIGINT NOT NULL,
			address TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			watch_income BOOLEAN NOT NULL DEFAULT TRUE,
			watch_expense BOOLEAN NOT NULL DEFAULT TRUE,
			notify_trx BOOLEAN NOT NULL DEFAULT TRUE,
			min_notify_amount TEXT NOT NULL DEFAULT '0',
			active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(bot_id, chat_id, owner_user_id, address)
		)`,
		`ALTER TABLE chain_watcher_subscriptions ADD COLUMN IF NOT EXISTS chat_id BIGINT NOT NULL DEFAULT 0`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_chain_watcher_subscriptions_identity
			ON chain_watcher_subscriptions(bot_id, chat_id, owner_user_id, address)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_subscriptions_active_address
			ON chain_watcher_subscriptions(active, address, bot_id, chat_id, owner_user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_subscriptions_bot_active
			ON chain_watcher_subscriptions(bot_id, active, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS chain_watcher_events (
			event_id TEXT PRIMARY KEY,
			tx_hash TEXT NOT NULL,
			contract TEXT NOT NULL DEFAULT '',
			from_address TEXT NOT NULL,
			to_address TEXT NOT NULL,
			value TEXT NOT NULL,
			token_symbol TEXT NOT NULL DEFAULT '',
			token_address TEXT NOT NULL DEFAULT '',
			token_decimals INTEGER NOT NULL DEFAULT 6,
			block_timestamp BIGINT NOT NULL,
			confirmed BOOLEAN NOT NULL DEFAULT FALSE,
			source TEXT NOT NULL DEFAULT '',
			event_index TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL
		)`,
		`ALTER TABLE chain_watcher_events ADD COLUMN IF NOT EXISTS event_index TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_events_tx
			ON chain_watcher_events(tx_hash, block_timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_events_created
			ON chain_watcher_events(created_at)`,
		`CREATE TABLE IF NOT EXISTS chain_watcher_matched_events (
			delivery_id TEXT PRIMARY KEY,
			event_id TEXT NOT NULL,
			bot_id TEXT NOT NULL,
			chat_id BIGINT NOT NULL DEFAULT 0,
			owner_user_id BIGINT NOT NULL,
			watch_address TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			direction TEXT NOT NULL,
			tx_hash TEXT NOT NULL,
			from_address TEXT NOT NULL,
			to_address TEXT NOT NULL,
			value TEXT NOT NULL,
			token_symbol TEXT NOT NULL DEFAULT '',
			token_address TEXT NOT NULL DEFAULT '',
			token_decimals INTEGER NOT NULL DEFAULT 6,
			block_timestamp BIGINT NOT NULL,
			confirmed BOOLEAN NOT NULL DEFAULT FALSE,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			delivered_at TIMESTAMPTZ
		)`,
		`ALTER TABLE chain_watcher_matched_events ADD COLUMN IF NOT EXISTS chat_id BIGINT NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_matched_due
			ON chain_watcher_matched_events(bot_id, status, next_attempt_at, created_at)
			WHERE status IN ('pending', 'delivering')`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_matched_event
			ON chain_watcher_matched_events(event_id, bot_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_matched_created
			ON chain_watcher_matched_events(created_at)`,
		`CREATE TABLE IF NOT EXISTS notification_outbox (
			id BIGSERIAL PRIMARY KEY,
			kind TEXT NOT NULL,
			dedupe_key TEXT NOT NULL UNIQUE,
			chat_id BIGINT NOT NULL,
			text TEXT NOT NULL,
			parse_mode TEXT NOT NULL DEFAULT '',
			disable_preview BOOLEAN NOT NULL DEFAULT FALSE,
			reply_to_message_id BIGINT NOT NULL DEFAULT 0,
			reply_markup_json TEXT NOT NULL DEFAULT '',
			reference_kind TEXT NOT NULL DEFAULT '',
			reference_id BIGINT NOT NULL DEFAULT 0,
			priority INTEGER NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TIMESTAMPTZ NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			sent_at TIMESTAMPTZ
		)`,
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS reply_to_message_id BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS reply_markup_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS reference_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS reference_id BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 1`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_due
			ON notification_outbox(status, next_attempt_at, id)
			WHERE status IN ('pending', 'failed', 'sending')`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_due_priority
			ON notification_outbox(priority, next_attempt_at, id)
			WHERE status IN ('pending', 'failed', 'sending')`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_chat
			ON notification_outbox(chat_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_reference
			ON notification_outbox(reference_kind, reference_id)
			WHERE reference_kind <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_status_priority
			ON notification_outbox(status, priority, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_status_updated
			ON notification_outbox(status, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_pending_created
			ON notification_outbox(created_at)
			WHERE status='pending'`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_status_sent_at
			ON notification_outbox(status, sent_at)
			WHERE status='sent'`,
		`CREATE INDEX IF NOT EXISTS idx_notification_outbox_failed_updated
			ON notification_outbox(updated_at)
			WHERE status='failed'`,
	}
	for _, statement := range statements {
		if _, err := s.pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations(version, applied_at)
		VALUES('2.1.0', NOW())
		ON CONFLICT(version) DO NOTHING`); err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations(version, applied_at)
		VALUES('2.2.0', NOW())
		ON CONFLICT(version) DO NOTHING`); err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations(version, applied_at)
		VALUES('2.3.0', NOW())
		ON CONFLICT(version) DO NOTHING`); err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations(version, applied_at)
		VALUES('2.4.1', NOW())
		ON CONFLICT(version) DO NOTHING`); err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO schema_migrations(version, applied_at)
		VALUES('2.4.2', NOW())
		ON CONFLICT(version) DO NOTHING`); err != nil {
		return fmt.Errorf("record schema migration: %w", err)
	}
	return nil
}

func (s *Store) ClaimUpdate(ctx context.Context, updateID int64, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO processed_updates(update_id, processed_at)
		 VALUES($1, $2)
		 ON CONFLICT DO NOTHING`,
		updateID,
		now,
	)
	return tag.RowsAffected() == 1, err
}

func (s *Store) EnsureGroup(ctx context.Context, chatID int64, title string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO groups(chat_id, title, created_at, updated_at)
		VALUES($1, $2, $3, $4)
		ON CONFLICT(chat_id) DO UPDATE SET title=excluded.title, updated_at=excluded.updated_at`,
		chatID, title, now, now)
	return err
}

func (s *Store) TouchUser(ctx context.Context, chatID int64, user User, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO users(chat_id, user_id, username, display_name, last_seen_at)
		VALUES($1, $2, $3, $4, $5)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			username=excluded.username,
			display_name=excluded.display_name,
			last_seen_at=excluded.last_seen_at`,
		chatID, user.ID, user.Username, user.DisplayName, now)
	return err
}

func (s *Store) FindUserByUsername(ctx context.Context, chatID int64, username string) (User, bool, error) {
	username = NormalizeUsername(username)
	if username == "" {
		return User{}, false, nil
	}
	row := s.pool.QueryRow(ctx, `SELECT user_id, username, display_name
		FROM users
		WHERE chat_id=$1 AND lower(username)=$2
		LIMIT 1`, chatID, username)
	var user User
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	return user, err == nil, err
}

func (s *Store) GetUser(ctx context.Context, chatID, userID int64) (User, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT user_id, username, display_name
		FROM users
		WHERE chat_id=$1 AND user_id=$2
		LIMIT 1`, chatID, userID)
	var user User
	err := row.Scan(&user.ID, &user.Username, &user.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	return user, err == nil, err
}

func (s *Store) GetGroup(ctx context.Context, chatID int64) (Group, error) {
	row := s.pool.QueryRow(ctx, `SELECT chat_id, title, active, active_day_key, active_expires_day_key, active_period_started_at, business_open, owner_user_id,
		deposit_rate, payout_rate, deposit_exchange_rate, payout_exchange_rate,
		exchange_rate_source, exchange_rate_rank, exchange_rate_offset, fee_rate,
		cutoff_hour, all_members_can_record, created_at, updated_at
		FROM groups WHERE chat_id = $1`, chatID)
	return scanGroup(row)
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.pool.Query(ctx, `SELECT chat_id, title, active, active_day_key, active_expires_day_key, active_period_started_at, business_open, owner_user_id,
		deposit_rate, payout_rate, deposit_exchange_rate, payout_exchange_rate,
		exchange_rate_source, exchange_rate_rank, exchange_rate_offset, fee_rate,
		cutoff_hour, all_members_can_record, created_at, updated_at
		FROM groups
		WHERE chat_id < 0
		ORDER BY updated_at DESC, title ASC, chat_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func scanGroup(scanner recordScanner) (Group, error) {
	var g Group
	err := scanner.Scan(&g.ChatID, &g.Title, &g.Active, &g.ActiveDayKey, &g.ActiveExpiresDayKey, &g.ActivePeriodStartedAt, &g.BusinessOpen, &g.OwnerUserID,
		&g.DepositRate, &g.PayoutRate, &g.DepositExchangeRate, &g.PayoutExchangeRate,
		&g.ExchangeRateSource, &g.ExchangeRateRank, &g.ExchangeRateOffset, &g.FeeRate,
		&g.CutoffHour, &g.AllMembersCanRecord, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

func (s *Store) SetGroupActive(ctx context.Context, chatID int64, active bool, activeDayKey string, now time.Time) error {
	activeExpiresDayKey := activeDayKey
	if !active {
		activeExpiresDayKey = ""
	}
	return s.SetGroupActivePeriod(ctx, chatID, active, activeDayKey, activeExpiresDayKey, now)
}

func (s *Store) SetGroupActivePeriod(ctx context.Context, chatID int64, active bool, activeDayKey, activeExpiresDayKey string, now time.Time) error {
	if !active {
		activeDayKey = ""
		activeExpiresDayKey = ""
	}
	_, err := s.pool.Exec(ctx, `UPDATE groups SET
		active=$1,
		active_day_key=$2,
		active_expires_day_key=$3,
		active_period_started_at=CASE
			WHEN $1 AND (NOT active OR active_day_key<>$2 OR active_period_started_at='1970-01-01 00:00:00+00'::timestamptz) THEN $4
			WHEN NOT $1 THEN '1970-01-01 00:00:00+00'::timestamptz
			ELSE active_period_started_at
		END,
		updated_at=$4 WHERE chat_id=$5`,
		active, activeDayKey, activeExpiresDayKey, now, chatID)
	return err
}

func (s *Store) SetGroupCutoffState(ctx context.Context, chatID int64, cutoffHour int, active bool, activeDayKey, activeExpiresDayKey string, now time.Time) error {
	if !active {
		activeDayKey = ""
		activeExpiresDayKey = ""
	}
	_, err := s.pool.Exec(ctx, `UPDATE groups SET cutoff_hour=$1, active=$2, active_day_key=$3, active_expires_day_key=$4,
		active_period_started_at=CASE
			WHEN $2 AND active_period_started_at<='1970-01-01 00:00:00+00'::timestamptz THEN $5
			WHEN NOT $2 THEN '1970-01-01 00:00:00+00'::timestamptz
			ELSE active_period_started_at
		END,
		updated_at=$5 WHERE chat_id=$6`,
		cutoffHour, active, activeDayKey, activeExpiresDayKey, now, chatID)
	return err
}

func (s *Store) SetGroupBusinessOpen(ctx context.Context, chatID int64, open bool, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE groups SET business_open=$1, updated_at=$2 WHERE chat_id=$3`,
		open, now, chatID)
	return err
}

func (s *Store) SetGroupFeeRate(ctx context.Context, chatID int64, feeRate string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE groups SET fee_rate=$1, deposit_rate=$1, payout_rate=$1, updated_at=$2 WHERE chat_id=$3`,
		feeRate, now, chatID)
	return err
}

func (s *Store) SetGroupExchangeRate(ctx context.Context, chatID int64, rate string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE groups SET
			deposit_exchange_rate=$1,
			payout_exchange_rate=$1,
			exchange_rate_source='',
			exchange_rate_rank=0,
			exchange_rate_offset='',
			updated_at=$2
		WHERE chat_id=$3`,
		rate, now, chatID)
	return err
}

func (s *Store) SetGroupRealtimeExchangeRate(ctx context.Context, chatID int64, rate, source string, rank int, offset string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE groups SET
			deposit_exchange_rate=$1,
			payout_exchange_rate=$1,
			exchange_rate_source=$2,
			exchange_rate_rank=$3,
			exchange_rate_offset=$4,
			updated_at=$5
		WHERE chat_id=$6`,
		rate, source, rank, offset, now, chatID)
	return err
}

func (s *Store) SetGroupOwner(ctx context.Context, chatID int64, user User, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err := tx.Exec(ctx, `UPDATE groups SET owner_user_id=$1, updated_at=$2 WHERE chat_id=$3`, user.ID, now, chatID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM operators WHERE chat_id=$1 AND role='owner'`, chatID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operators(chat_id, user_id, role, added_by, created_at)
		VALUES($1, $2, 'owner', $3, $4)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET role='owner', added_by=excluded.added_by, created_at=excluded.created_at`,
		chatID, user.ID, user.ID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AddOperator(ctx context.Context, chatID int64, user User, addedBy int64, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO operators(chat_id, user_id, role, added_by, created_at)
		VALUES($1, $2, 'operator', $3, $4)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET role='operator', added_by=excluded.added_by, created_at=excluded.created_at`,
		chatID, user.ID, addedBy, now)
	return err
}

func (s *Store) RemoveOperator(ctx context.Context, chatID, userID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM operators WHERE chat_id=$1 AND user_id=$2 AND role <> 'owner'`, chatID, userID)
	return tag.RowsAffected() > 0, err
}

func (s *Store) IsOperator(ctx context.Context, chatID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT 1 FROM operators WHERE chat_id=$1 AND user_id=$2 LIMIT 1`, chatID, userID)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) IsOwner(ctx context.Context, chatID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT 1 FROM groups WHERE chat_id=$1 AND owner_user_id=$2 LIMIT 1`, chatID, userID)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) ListOperators(ctx context.Context, chatID int64) ([]Operator, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.chat_id, o.user_id, o.role, o.added_by, o.created_at,
		COALESCE(u.username, ''), COALESCE(u.display_name, '')
		FROM operators o
		LEFT JOIN users u ON u.chat_id = o.chat_id AND u.user_id = o.user_id
		WHERE o.chat_id=$1
		ORDER BY CASE WHEN o.role='owner' THEN 0 ELSE 1 END, o.created_at ASC, o.user_id ASC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var operators []Operator
	for rows.Next() {
		var op Operator
		if err := rows.Scan(&op.ChatID, &op.UserID, &op.Role, &op.AddedBy, &op.CreatedAt, &op.Username, &op.DisplayName); err != nil {
			return nil, err
		}
		operators = append(operators, op)
	}
	return operators, rows.Err()
}

func (s *Store) ListUsersForMention(ctx context.Context, chatID int64, limit int) ([]User, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `SELECT user_id, username, display_name
		FROM users
		WHERE chat_id=$1
		ORDER BY last_seen_at DESC, user_id ASC
		LIMIT $2`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Username, &user.DisplayName); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func normalizeGlobalOperatorLevel(level string) (string, error) {
	level = strings.TrimSpace(level)
	switch level {
	case "primary", "secondary":
		return level, nil
	default:
		return "", errors.New("invalid global operator level")
	}
}

func (s *Store) IsGlobalOperator(ctx context.Context, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT 1 FROM global_operators
		WHERE user_id=$1 AND status='active' AND level IN ('primary', 'secondary') LIMIT 1`, userID)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) GetGlobalOperatorLevel(ctx context.Context, userID int64) (string, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT level FROM global_operators
		WHERE user_id=$1 AND status='active' AND level IN ('primary', 'secondary') LIMIT 1`, userID)
	var level string
	err := row.Scan(&level)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return level, err == nil, err
}

func (s *Store) GetGlobalOperator(ctx context.Context, userID int64) (GlobalOperator, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT user_id, level, status, COALESCE(parent_user_id, 0),
		remark, created_by, created_at, COALESCE(disabled_by, 0), disabled_at
		FROM global_operators WHERE user_id=$1`, userID)
	var op GlobalOperator
	var disabledAt pgtype.Timestamptz
	if err := row.Scan(&op.UserID, &op.Level, &op.Status, &op.ParentUserID, &op.Remark,
		&op.CreatedBy, &op.CreatedAt, &op.DisabledBy, &disabledAt); errors.Is(err, pgx.ErrNoRows) {
		return GlobalOperator{}, false, nil
	} else if err != nil {
		return GlobalOperator{}, false, err
	}
	if disabledAt.Valid {
		t := disabledAt.Time
		op.DisabledAt = &t
	}
	return op, true, nil
}

func validateGlobalOperatorParent(ctx context.Context, tx pgx.Tx, userID int64, level string, parentUserID int64) error {
	if level == "primary" {
		if parentUserID != 0 {
			return errors.New("primary global operator cannot have a parent")
		}
		return nil
	}
	if parentUserID <= 0 || parentUserID == userID {
		return errors.New("secondary global operator requires a different active primary parent")
	}
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM global_operators
		WHERE user_id=$1 AND level='primary' AND status='active' LIMIT 1`, parentUserID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("secondary global operator parent is not an active primary")
	}
	return err
}

func insertPermissionAudit(ctx context.Context, tx pgx.Tx, event PermissionAuditEvent) error {
	_, err := tx.Exec(ctx, `INSERT INTO permission_audit_events(
		actor_user_id, subject_type, subject_user_id, action, level, parent_user_id,
		target_type, chat_id, group_name, created_at
	) VALUES($1, $2, $3, $4, $5, NULLIF($6, 0), $7, $8, $9, $10)`,
		event.ActorUserID, event.SubjectType, event.SubjectUserID, event.Action, event.Level,
		event.ParentUserID, event.TargetType, event.ChatID, event.GroupName, event.CreatedAt)
	return err
}

func (s *Store) UpsertGlobalOperator(ctx context.Context, userID int64, level string, parentUserID, createdBy int64, remark string, now time.Time) error {
	if userID <= 0 {
		return errors.New("global operator user id is empty")
	}
	level, err := normalizeGlobalOperatorLevel(level)
	if err != nil {
		return err
	}
	remark = strings.TrimSpace(remark)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err := validateGlobalOperatorParent(ctx, tx, userID, level, parentUserID); err != nil {
		return err
	}
	var oldLevel, oldStatus string
	var oldParentUserID int64
	err = tx.QueryRow(ctx, `SELECT level, status, COALESCE(parent_user_id, 0)
		FROM global_operators WHERE user_id=$1 FOR UPDATE`, userID).
		Scan(&oldLevel, &oldStatus, &oldParentUserID)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO global_operators(user_id, level, status, parent_user_id, created_by, created_at, remark)
		VALUES($1, $2, 'active', NULLIF($3, 0), $4, $5, $6)
		ON CONFLICT(user_id) DO UPDATE SET
			level=excluded.level,
			status='active',
			parent_user_id=excluded.parent_user_id,
			remark=excluded.remark,
			disabled_by=NULL,
			disabled_at=NULL`,
		userID, level, parentUserID, createdBy, now, remark); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO broadcast_operators(user_id, status, created_by, remark, created_at, updated_at)
		VALUES($1, 'active', $2, $3, $4, $5)
		ON CONFLICT(user_id) DO UPDATE SET status='active', remark=excluded.remark, updated_at=excluded.updated_at`,
		userID, createdBy, remark, now, now); err != nil {
		return err
	}
	action := "created"
	if exists && oldStatus == "disabled" {
		action = "reenabled"
	} else if exists && oldLevel != level {
		action = "level_changed"
	} else if exists && oldParentUserID != parentUserID {
		action = "parent_changed"
	} else if exists {
		action = "updated"
	}
	if err := insertPermissionAudit(ctx, tx, PermissionAuditEvent{
		ActorUserID: createdBy, SubjectType: "global_operator", SubjectUserID: userID,
		Action: action, Level: level, ParentUserID: parentUserID, CreatedAt: now,
	}); err != nil {
		return err
	}
	if exists && oldStatus == "disabled" && oldLevel != level {
		if err := insertPermissionAudit(ctx, tx, PermissionAuditEvent{
			ActorUserID: createdBy, SubjectType: "global_operator", SubjectUserID: userID,
			Action: "level_changed", Level: level, ParentUserID: parentUserID, CreatedAt: now,
		}); err != nil {
			return err
		}
	}
	if exists && oldStatus == "disabled" && oldParentUserID != parentUserID {
		if err := insertPermissionAudit(ctx, tx, PermissionAuditEvent{
			ActorUserID: createdBy, SubjectType: "global_operator", SubjectUserID: userID,
			Action: "parent_changed", Level: level, ParentUserID: parentUserID, CreatedAt: now,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) DisableGlobalOperator(ctx context.Context, userID, disabledBy int64, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	var level string
	var parentUserID int64
	err = tx.QueryRow(ctx, `SELECT level, COALESCE(parent_user_id, 0) FROM global_operators
		WHERE user_id=$1 AND status='active' FOR UPDATE`, userID).Scan(&level, &parentUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if level == "primary" {
		rows, err := tx.Query(ctx, `SELECT user_id FROM global_operators
			WHERE parent_user_id=$1 AND level='secondary' AND status='active'
			ORDER BY user_id FOR UPDATE`, userID)
		if err != nil {
			return false, err
		}
		var childIDs []int64
		for rows.Next() {
			var childID int64
			if err := rows.Scan(&childID); err != nil {
				rows.Close()
				return false, err
			}
			childIDs = append(childIDs, childID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false, err
		}
		rows.Close()
		for _, childID := range childIDs {
			if _, err := tx.Exec(ctx, `UPDATE global_operators
				SET status='disabled', disabled_by=$1, disabled_at=$2 WHERE user_id=$3`, disabledBy, now, childID); err != nil {
				return false, err
			}
			if _, err := tx.Exec(ctx, `UPDATE broadcast_operators SET status='disabled', updated_at=$1 WHERE user_id=$2`, now, childID); err != nil {
				return false, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM broadcast_operator_permissions WHERE user_id=$1`, childID); err != nil {
				return false, err
			}
			if err := insertPermissionAudit(ctx, tx, PermissionAuditEvent{
				ActorUserID: disabledBy, SubjectType: "global_operator", SubjectUserID: childID,
				Action: "disabled", Level: "secondary", ParentUserID: userID, CreatedAt: now,
			}); err != nil {
				return false, err
			}
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE global_operators
		SET status='disabled', disabled_by=$1, disabled_at=$2
		WHERE user_id=$3 AND status <> 'disabled'`, disabledBy, now, userID)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE broadcast_operators SET status='disabled', updated_at=$1 WHERE user_id=$2 AND status <> 'disabled'`,
		now, userID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM broadcast_operator_permissions WHERE user_id=$1`, userID); err != nil {
		return false, err
	}
	if err := insertPermissionAudit(ctx, tx, PermissionAuditEvent{
		ActorUserID: disabledBy, SubjectType: "global_operator", SubjectUserID: userID,
		Action: "disabled", Level: level, ParentUserID: parentUserID, CreatedAt: now,
	}); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, tx.Commit(ctx)
}

func (s *Store) ListGlobalOperators(ctx context.Context) ([]GlobalOperator, error) {
	rows, err := s.pool.Query(ctx, `SELECT g.user_id, g.level, g.status, COALESCE(g.parent_user_id, 0),
			g.remark, g.created_by, g.created_at, COALESCE(g.disabled_by, 0), g.disabled_at,
			COALESCE(b.private_cleanup_enabled, FALSE),
			COALESCE(b.private_cleanup_time, ''),
			COALESCE(b.private_cleanup_last_run_date, ''),
			COALESCE(b.private_cleanup_bot_after_seconds, 0),
			COALESCE(b.private_cleanup_incoming_enabled, FALSE),
			COALESCE(b.private_cleanup_incoming_after_seconds, 0),
			COALESCE(b.private_cleanup_scope, ''),
			COALESCE(b.updated_at, g.created_at)
		FROM global_operators g
		LEFT JOIN broadcast_operators b ON b.user_id=g.user_id
		ORDER BY g.status ASC, CASE WHEN g.level='primary' THEN 0 ELSE 1 END, g.created_at ASC, g.user_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var operators []GlobalOperator
	for rows.Next() {
		var op GlobalOperator
		var disabledAt pgtype.Timestamptz
		if err := rows.Scan(
			&op.UserID,
			&op.Level,
			&op.Status,
			&op.ParentUserID,
			&op.Remark,
			&op.CreatedBy,
			&op.CreatedAt,
			&op.DisabledBy,
			&disabledAt,
			&op.PrivateCleanupEnabled,
			&op.PrivateCleanupTime,
			&op.PrivateCleanupLastRunDate,
			&op.PrivateCleanupBotDeleteAfterSeconds,
			&op.PrivateCleanupIncomingEnabled,
			&op.PrivateCleanupIncomingAfterSeconds,
			&op.PrivateCleanupScope,
			&op.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if disabledAt.Valid {
			t := disabledAt.Time
			op.DisabledAt = &t
		}
		operators = append(operators, op)
	}
	return operators, rows.Err()
}

func (s *Store) ListPermissionAuditEvents(ctx context.Context, subjectUserID int64, limit int) ([]PermissionAuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, actor_user_id, subject_type, subject_user_id,
		action, level, COALESCE(parent_user_id, 0), target_type, chat_id, group_name, created_at
		FROM permission_audit_events
		WHERE $1=0 OR subject_user_id=$1
		ORDER BY created_at DESC, id DESC LIMIT $2`, subjectUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]PermissionAuditEvent, 0, limit)
	for rows.Next() {
		var event PermissionAuditEvent
		if err := rows.Scan(&event.ID, &event.ActorUserID, &event.SubjectType, &event.SubjectUserID,
			&event.Action, &event.Level, &event.ParentUserID, &event.TargetType, &event.ChatID,
			&event.GroupName, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) SetBroadcastOperatorPrivateCleanup(ctx context.Context, userID int64, enabled bool, cleanupTime string, lastRunDate string, now time.Time) (bool, error) {
	return s.SetBroadcastOperatorPrivateCleanupSettings(ctx, userID, PrivateCleanupSettings{
		Enabled:          enabled,
		DailyTime:        cleanupTime,
		DailyLastRunDate: lastRunDate,
		Scope:            DefaultPrivateCleanupScope(),
	}, now)
}

func (s *Store) SetBroadcastOperatorPrivateCleanupSettings(ctx context.Context, userID int64, settings PrivateCleanupSettings, now time.Time) (bool, error) {
	settings = NormalizePrivateCleanupSettings(settings)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	tag, err := tx.Exec(ctx, `UPDATE broadcast_operators
		SET private_cleanup_enabled=$1,
			private_cleanup_time=$2,
			private_cleanup_last_run_date=$3,
			private_cleanup_bot_after_seconds=$4,
			private_cleanup_incoming_enabled=$5,
			private_cleanup_incoming_after_seconds=$6,
			private_cleanup_scope=$7,
			updated_at=$8
		WHERE user_id=$9
		  AND status='active'
		  AND EXISTS (
			SELECT 1 FROM global_operators g
			WHERE g.user_id=$9 AND g.status='active' AND g.level IN ('primary', 'secondary')
		  )`,
		settings.Enabled,
		settings.DailyTime,
		settings.DailyLastRunDate,
		settings.BotDeleteAfter,
		settings.IncomingEnabled,
		settings.IncomingDeleteAfter,
		settings.Scope,
		now,
		userID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	if !settings.Enabled {
		if _, err := tx.Exec(ctx, `UPDATE private_chat_messages
			SET deleted_at=$1, last_error=''
			WHERE operator_user_id=$2 AND deleted_at IS NULL`, now, userID); err != nil {
			return false, err
		}
	} else if !settings.IncomingEnabled {
		if _, err := tx.Exec(ctx, `UPDATE private_chat_messages
			SET deleted_at=$1, last_error=''
			WHERE operator_user_id=$2 AND deleted_at IS NULL AND direction='incoming'`, now, userID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func (s *Store) IsPrivateCleanupEnabled(ctx context.Context, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT 1
		FROM broadcast_operators b
		JOIN global_operators g ON g.user_id=b.user_id
		WHERE b.user_id=$1
		  AND b.status='active'
		  AND g.status='active'
		  AND g.level IN ('primary', 'secondary')
		  AND b.private_cleanup_enabled=TRUE
		  AND (
			b.private_cleanup_time <> ''
			OR b.private_cleanup_bot_after_seconds > 0
			OR b.private_cleanup_incoming_enabled=TRUE
		  )
		LIMIT 1`, userID)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) GetPrivateCleanupSettings(ctx context.Context, userID int64) (PrivateCleanupSettings, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT private_cleanup_enabled, private_cleanup_time, private_cleanup_last_run_date,
			private_cleanup_bot_after_seconds, private_cleanup_incoming_enabled,
			private_cleanup_incoming_after_seconds, private_cleanup_scope
		FROM broadcast_operators b
		JOIN global_operators g ON g.user_id=b.user_id
		WHERE b.user_id=$1 AND b.status='active'
		  AND g.status='active' AND g.level IN ('primary', 'secondary')
		LIMIT 1`, userID)
	var settings PrivateCleanupSettings
	err := row.Scan(
		&settings.Enabled,
		&settings.DailyTime,
		&settings.DailyLastRunDate,
		&settings.BotDeleteAfter,
		&settings.IncomingEnabled,
		&settings.IncomingDeleteAfter,
		&settings.Scope,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PrivateCleanupSettings{}, false, nil
	}
	if err != nil {
		return PrivateCleanupSettings{}, false, err
	}
	return NormalizePrivateCleanupSettings(settings), true, nil
}

func (s *Store) RecordPrivateChatMessage(ctx context.Context, msg PrivateChatMessage) error {
	if msg.OperatorUserID <= 0 || msg.ChatID <= 0 || msg.MessageID <= 0 {
		return nil
	}
	direction := strings.TrimSpace(msg.Direction)
	if direction == "" {
		direction = "unknown"
	}
	category := strings.TrimSpace(msg.Category)
	if category == "" {
		category = "private"
	}
	createdAt := msg.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO private_chat_messages(
			operator_user_id, chat_id, message_id, direction, category,
			cleanup_after_seconds, due_at, created_at
		) VALUES($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT(chat_id, message_id) DO NOTHING`,
		msg.OperatorUserID, msg.ChatID, msg.MessageID, direction, category, msg.CleanupAfterSeconds, msg.DueAt, createdAt)
	return err
}

func (s *Store) PurgePrivateChatMessages(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM private_chat_messages WHERE deleted_at IS NOT NULL AND deleted_at < $1`, cutoff)
	return tag.RowsAffected(), err
}

func (s *Store) ListDuePrivateCleanupTargets(ctx context.Context, nowMinutes int, today string) ([]PrivateCleanupTarget, error) {
	rows, err := s.pool.Query(ctx, `SELECT b.user_id, b.private_cleanup_time
		FROM broadcast_operators b
		JOIN global_operators g ON g.user_id=b.user_id
		WHERE b.status='active'
		  AND g.status='active'
		  AND g.level IN ('primary', 'secondary')
		  AND b.private_cleanup_enabled=TRUE
		  AND b.private_cleanup_time <> ''
		  AND b.private_cleanup_last_run_date <> $1
		ORDER BY b.user_id ASC`, strings.TrimSpace(today))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []PrivateCleanupTarget
	for rows.Next() {
		var target PrivateCleanupTarget
		if err := rows.Scan(&target.UserID, &target.CleanupTime); err != nil {
			return nil, err
		}
		minutes, ok := CleanupTimeMinutes(target.CleanupTime)
		if ok && minutes <= nowMinutes {
			targets = append(targets, target)
		}
	}
	return targets, rows.Err()
}

func (s *Store) ListPrivateChatMessagesForCleanup(ctx context.Context, operatorUserID int64, limit int) ([]PrivateChatMessage, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `SELECT id, operator_user_id, chat_id, message_id, direction, category,
			cleanup_after_seconds, due_at, created_at, deleted_at, last_error
		FROM private_chat_messages
		WHERE operator_user_id=$1 AND deleted_at IS NULL
		ORDER BY id ASC
		LIMIT $2`, operatorUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []PrivateChatMessage
	for rows.Next() {
		var msg PrivateChatMessage
		if err := rows.Scan(
			&msg.ID,
			&msg.OperatorUserID,
			&msg.ChatID,
			&msg.MessageID,
			&msg.Direction,
			&msg.Category,
			&msg.CleanupAfterSeconds,
			&msg.DueAt,
			&msg.CreatedAt,
			&msg.DeletedAt,
			&msg.LastError,
		); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func (s *Store) ListDuePrivateChatMessagesForCleanup(ctx context.Context, now time.Time, limit int) ([]PrivateChatMessage, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `SELECT id, operator_user_id, chat_id, message_id, direction, category,
			cleanup_after_seconds, due_at, created_at, deleted_at, last_error
		FROM private_chat_messages
		WHERE deleted_at IS NULL AND due_at IS NOT NULL AND due_at <= $1
		ORDER BY due_at ASC, id ASC
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []PrivateChatMessage
	for rows.Next() {
		var msg PrivateChatMessage
		if err := rows.Scan(
			&msg.ID,
			&msg.OperatorUserID,
			&msg.ChatID,
			&msg.MessageID,
			&msg.Direction,
			&msg.Category,
			&msg.CleanupAfterSeconds,
			&msg.DueAt,
			&msg.CreatedAt,
			&msg.DeletedAt,
			&msg.LastError,
		); err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func (s *Store) MarkPrivateChatMessageCleanup(ctx context.Context, id int64, lastError string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE private_chat_messages
		SET deleted_at=$1, last_error=$2
		WHERE id=$3 AND deleted_at IS NULL`, now, trimError(lastError, 500), id)
	return err
}

func (s *Store) MarkPrivateCleanupRun(ctx context.Context, userID int64, runDate string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE broadcast_operators
		SET private_cleanup_last_run_date=$1, updated_at=$2
		WHERE user_id=$3`, strings.TrimSpace(runDate), now, userID)
	return err
}

func CleanupTimeMinutes(value string) (int, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, ".", ":"))
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hour < 0 || hour > 23 {
		return 0, false
	}
	minute, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func DefaultPrivateCleanupScope() string {
	return "broadcast,quick_reply,menu"
}

func NormalizePrivateCleanupSettings(settings PrivateCleanupSettings) PrivateCleanupSettings {
	settings.DailyTime = strings.TrimSpace(settings.DailyTime)
	settings.DailyLastRunDate = strings.TrimSpace(settings.DailyLastRunDate)
	settings.Scope = strings.TrimSpace(settings.Scope)
	if settings.Scope == "" {
		settings.Scope = DefaultPrivateCleanupScope()
	}
	if settings.BotDeleteAfter < 0 {
		settings.BotDeleteAfter = 0
	}
	if settings.IncomingDeleteAfter < 0 {
		settings.IncomingDeleteAfter = 0
	}
	if !settings.Enabled {
		settings.DailyTime = ""
		settings.DailyLastRunDate = ""
		settings.BotDeleteAfter = 0
		settings.IncomingEnabled = false
		settings.IncomingDeleteAfter = 0
	}
	if !settings.IncomingEnabled {
		settings.IncomingDeleteAfter = 0
	}
	return settings
}

func trimError(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func (s *Store) CreateAdminLoginTicket(ctx context.Context, tokenHash string, userID int64, role string, expiresAt, now time.Time) error {
	tokenHash = strings.TrimSpace(tokenHash)
	role = strings.TrimSpace(role)
	if tokenHash == "" {
		return errors.New("admin login ticket token hash is empty")
	}
	if userID <= 0 {
		return errors.New("admin login ticket user id is empty")
	}
	if role == "" {
		return errors.New("admin login ticket role is empty")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO admin_login_tickets(token_hash, user_id, role, expires_at, created_at)
		VALUES($1, $2, $3, $4, $5)
		ON CONFLICT(token_hash) DO NOTHING`, tokenHash, userID, role, expiresAt, now)
	return err
}

func (s *Store) GetAdminLoginTicket(ctx context.Context, tokenHash string, now time.Time) (AdminLoginTicket, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT token_hash, user_id, role, expires_at, used_at, created_at
		FROM admin_login_tickets
		WHERE token_hash=$1 AND used_at IS NULL AND expires_at > $2`, strings.TrimSpace(tokenHash), now)
	var ticket AdminLoginTicket
	err := row.Scan(&ticket.TokenHash, &ticket.UserID, &ticket.Role, &ticket.ExpiresAt, &ticket.UsedAt, &ticket.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminLoginTicket{}, false, nil
	}
	return ticket, err == nil, err
}

func (s *Store) ConsumeAdminLoginTicket(ctx context.Context, tokenHash string, now time.Time) (AdminLoginTicket, bool, error) {
	row := s.pool.QueryRow(ctx, `UPDATE admin_login_tickets
		SET used_at=$2
		WHERE token_hash=$1 AND used_at IS NULL AND expires_at > $2
		RETURNING token_hash, user_id, role, expires_at, used_at, created_at`, strings.TrimSpace(tokenHash), now)
	var ticket AdminLoginTicket
	err := row.Scan(&ticket.TokenHash, &ticket.UserID, &ticket.Role, &ticket.ExpiresAt, &ticket.UsedAt, &ticket.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminLoginTicket{}, false, nil
	}
	return ticket, err == nil, err
}

func (s *Store) CleanupAdminLoginTickets(ctx context.Context, cutoff time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM admin_login_tickets WHERE expires_at < $1 OR used_at IS NOT NULL`, cutoff)
	return err
}

func (s *Store) UpsertBroadcastGroup(ctx context.Context, name string, createdBy int64, now time.Time) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("broadcast group name is empty")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO broadcast_groups(name, created_by, created_at, updated_at)
		VALUES($1, $2, $3, $4)
		ON CONFLICT(name) DO UPDATE SET updated_at=excluded.updated_at`,
		name, createdBy, now, now)
	return err
}

func (s *Store) DeleteBroadcastGroup(ctx context.Context, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM broadcast_groups WHERE name=$1`, strings.TrimSpace(name))
	return tag.RowsAffected() > 0, err
}

func (s *Store) AddChatsToBroadcastGroup(ctx context.Context, name string, chatIDs []int64, now time.Time) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(chatIDs) == 0 {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(ctx, tx)
	count := 0
	for _, chatID := range uniqueInt64(chatIDs) {
		tag, err := tx.Exec(ctx, `INSERT INTO broadcast_group_chats(group_name, chat_id, created_at)
			SELECT $1, $2, $3
			WHERE EXISTS(SELECT 1 FROM broadcast_groups WHERE name=$1)
			  AND EXISTS(SELECT 1 FROM groups WHERE chat_id=$2)
			ON CONFLICT DO NOTHING`, name, chatID, now)
		if err != nil {
			return 0, err
		}
		count += int(tag.RowsAffected())
	}
	return count, tx.Commit(ctx)
}

func (s *Store) RemoveChatsFromBroadcastGroup(ctx context.Context, name string, chatIDs []int64) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(chatIDs) == 0 {
		return 0, nil
	}
	count := 0
	for _, chatID := range uniqueInt64(chatIDs) {
		tag, err := s.pool.Exec(ctx, `DELETE FROM broadcast_group_chats WHERE group_name=$1 AND chat_id=$2`, name, chatID)
		if err != nil {
			return 0, err
		}
		count += int(tag.RowsAffected())
	}
	return count, nil
}

func (s *Store) ListBroadcastGroups(ctx context.Context) ([]BroadcastGroup, error) {
	rows, err := s.pool.Query(ctx, `SELECT bg.name, bg.created_by, bg.created_at, bg.updated_at,
		COALESCE(g.chat_id, 0), COALESCE(g.title, '')
		FROM broadcast_groups bg
		LEFT JOIN broadcast_group_chats bgc ON bgc.group_name=bg.name
		LEFT JOIN groups g ON g.chat_id=bgc.chat_id
		ORDER BY bg.updated_at DESC, bg.name ASC, g.title ASC, g.chat_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]*BroadcastGroup{}
	var order []string
	for rows.Next() {
		var name string
		var createdBy int64
		var createdAt, updatedAt time.Time
		var chatID int64
		var title string
		if err := rows.Scan(&name, &createdBy, &createdAt, &updatedAt, &chatID, &title); err != nil {
			return nil, err
		}
		group := byName[name]
		if group == nil {
			group = &BroadcastGroup{Name: name, CreatedBy: createdBy, CreatedAt: createdAt, UpdatedAt: updatedAt}
			byName[name] = group
			order = append(order, name)
		}
		if chatID != 0 {
			group.ChatIDs = append(group.ChatIDs, chatID)
			group.ChatNames = append(group.ChatNames, title)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	groups := make([]BroadcastGroup, 0, len(order))
	for _, name := range order {
		groups = append(groups, *byName[name])
	}
	return groups, nil
}

func (s *Store) ListBroadcastGroupChats(ctx context.Context, name string) ([]Group, error) {
	rows, err := s.pool.Query(ctx, `SELECT g.chat_id, g.title, g.active, g.active_day_key, g.active_expires_day_key, g.active_period_started_at, g.business_open, g.owner_user_id,
		g.deposit_rate, g.payout_rate, g.deposit_exchange_rate, g.payout_exchange_rate,
		g.exchange_rate_source, g.exchange_rate_rank, g.exchange_rate_offset, g.fee_rate,
		g.cutoff_hour, g.all_members_can_record, g.created_at, g.updated_at
		FROM broadcast_group_chats bgc
		JOIN groups g ON g.chat_id=bgc.chat_id
		WHERE bgc.group_name=$1
		ORDER BY g.title ASC, g.chat_id ASC`, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) AddBroadcastPermission(ctx context.Context, userID int64, target string, chatID int64, groupName string, grantedBy int64, now time.Time) error {
	target = strings.TrimSpace(target)
	groupName = strings.TrimSpace(groupName)
	if target != "chat" && target != "group" {
		return errors.New("invalid broadcast permission target")
	}
	ok, err := s.IsGlobalOperator(ctx, userID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("broadcast permission user is not an active global operator")
	}
	if target == "chat" {
		if chatID == 0 {
			return errors.New("broadcast permission chat is empty")
		}
		groupName = ""
	} else {
		if groupName == "" {
			return errors.New("broadcast permission group is empty")
		}
		chatID = 0
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	tag, err := tx.Exec(ctx, `INSERT INTO broadcast_operator_permissions(user_id, target, chat_id, group_name, granted_by, created_at)
		VALUES($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`, userID, target, chatID, groupName, grantedBy, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		if err := insertPermissionAudit(ctx, tx, PermissionAuditEvent{
			ActorUserID: grantedBy, SubjectType: "broadcast_permission", SubjectUserID: userID,
			Action: "granted", TargetType: target, ChatID: chatID, GroupName: groupName, CreatedAt: now,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) RemoveBroadcastPermission(ctx context.Context, userID int64, target string, chatID int64, groupName string, revokedBy int64, now time.Time) (bool, error) {
	target = strings.TrimSpace(target)
	groupName = strings.TrimSpace(groupName)
	if target == "chat" {
		if chatID == 0 {
			return false, errors.New("broadcast permission chat is empty")
		}
		groupName = ""
	} else if target == "group" {
		if groupName == "" {
			return false, errors.New("broadcast permission group is empty")
		}
		chatID = 0
	} else {
		return false, errors.New("invalid broadcast permission target")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	tag, err := tx.Exec(ctx, `DELETE FROM broadcast_operator_permissions
		WHERE user_id=$1 AND target=$2 AND chat_id=$3 AND group_name=$4`, userID, target, chatID, groupName)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		if err := insertPermissionAudit(ctx, tx, PermissionAuditEvent{
			ActorUserID: revokedBy, SubjectType: "broadcast_permission", SubjectUserID: userID,
			Action: "revoked", TargetType: target, ChatID: chatID, GroupName: groupName, CreatedAt: now,
		}); err != nil {
			return false, err
		}
	}
	return tag.RowsAffected() > 0, tx.Commit(ctx)
}

func (s *Store) HasBroadcastPermissionScope(ctx context.Context, userID int64, target string, chatID int64, groupName string) (bool, error) {
	target = strings.TrimSpace(target)
	groupName = strings.TrimSpace(groupName)
	var row pgx.Row
	switch target {
	case "group":
		if groupName == "" {
			return false, nil
		}
		row = s.pool.QueryRow(ctx, `SELECT 1 FROM broadcast_operator_permissions
			WHERE user_id=$1 AND target='group' AND group_name=$2 LIMIT 1`, userID, groupName)
	case "chat":
		if chatID == 0 {
			return false, nil
		}
		row = s.pool.QueryRow(ctx, `SELECT 1
			FROM broadcast_operator_permissions p
			WHERE p.user_id=$1 AND (
				(p.target='chat' AND p.chat_id=$2)
				OR (p.target='group' AND EXISTS (
					SELECT 1 FROM broadcast_group_chats bgc
					WHERE bgc.group_name=p.group_name AND bgc.chat_id=$2
				))
			) LIMIT 1`, userID, chatID)
	default:
		return false, nil
	}
	var one int
	err := row.Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) ListBroadcastPermissions(ctx context.Context) ([]BroadcastPermission, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id, target, chat_id, group_name, granted_by, created_at
		FROM broadcast_operator_permissions
		ORDER BY user_id ASC, target ASC, group_name ASC, chat_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var permissions []BroadcastPermission
	for rows.Next() {
		var p BroadcastPermission
		if err := rows.Scan(&p.UserID, &p.Target, &p.ChatID, &p.GroupName, &p.GrantedBy, &p.CreatedAt); err != nil {
			return nil, err
		}
		permissions = append(permissions, p)
	}
	return permissions, rows.Err()
}

func (s *Store) ListAllowedBroadcastChats(ctx context.Context, userID int64, all bool) ([]Group, error) {
	if all {
		return s.ListGroups(ctx)
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT g.chat_id, g.title, g.active, g.active_day_key, g.active_expires_day_key, g.active_period_started_at, g.business_open, g.owner_user_id,
		g.deposit_rate, g.payout_rate, g.deposit_exchange_rate, g.payout_exchange_rate,
		g.exchange_rate_source, g.exchange_rate_rank, g.exchange_rate_offset, g.fee_rate,
		g.cutoff_hour, g.all_members_can_record, g.created_at, g.updated_at
		FROM groups g
		WHERE g.chat_id < 0
		  AND (
			EXISTS (
				SELECT 1 FROM broadcast_operator_permissions p
				WHERE p.user_id=$1 AND p.target='chat' AND p.chat_id=g.chat_id
			)
			OR EXISTS (
				SELECT 1 FROM broadcast_operator_permissions p
				JOIN broadcast_group_chats bgc ON bgc.group_name=p.group_name AND bgc.chat_id=g.chat_id
				WHERE p.user_id=$1 AND p.target='group'
			)
		  )
		ORDER BY g.title ASC, g.chat_id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) InsertBroadcastDelivery(ctx context.Context, d BroadcastDelivery) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO broadcast_deliveries(
		operator_user_id, source_chat_id, source_message_id, target_chat_id,
		target_title, target_message_id, mode, target_name, created_at
	) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9)
	ON CONFLICT(target_chat_id, target_message_id) DO UPDATE SET target_title=excluded.target_title
	RETURNING id`,
		d.OperatorUserID,
		d.SourceChatID,
		d.SourceMessageID,
		d.TargetChatID,
		d.TargetTitle,
		d.TargetMessageID,
		d.Mode,
		d.TargetName,
		d.CreatedAt,
	).Scan(&id)
	return id, err
}

func (s *Store) FindBroadcastDeliveryByTarget(ctx context.Context, chatID, messageID int64) (BroadcastDelivery, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, operator_user_id, source_chat_id, source_message_id,
		target_chat_id, target_title, target_message_id, mode, target_name, created_at, replaced_at
		FROM broadcast_deliveries
		WHERE target_chat_id=$1 AND target_message_id=$2`, chatID, messageID)
	return scanBroadcastDelivery(row)
}

func (s *Store) GetBroadcastDelivery(ctx context.Context, id int64) (BroadcastDelivery, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, operator_user_id, source_chat_id, source_message_id,
		target_chat_id, target_title, target_message_id, mode, target_name, created_at, replaced_at
		FROM broadcast_deliveries
		WHERE id=$1`, id)
	return scanBroadcastDelivery(row)
}

func scanBroadcastDelivery(scanner recordScanner) (BroadcastDelivery, bool, error) {
	var d BroadcastDelivery
	err := scanner.Scan(
		&d.ID,
		&d.OperatorUserID,
		&d.SourceChatID,
		&d.SourceMessageID,
		&d.TargetChatID,
		&d.TargetTitle,
		&d.TargetMessageID,
		&d.Mode,
		&d.TargetName,
		&d.CreatedAt,
		&d.ReplacedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return BroadcastDelivery{}, false, nil
	}
	return d, err == nil, err
}

func (s *Store) MarkBroadcastDeliveryReplaced(ctx context.Context, id int64, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE broadcast_deliveries
		SET replaced_at=$1
		WHERE id=$2 AND replaced_at IS NULL`, now, id)
	return tag.RowsAffected() > 0, err
}

func (s *Store) GetBroadcastReplaceSetting(ctx context.Context) (BroadcastReplaceSetting, error) {
	row := s.pool.QueryRow(ctx, `SELECT enabled, text, image_name, image_data, updated_at
		FROM broadcast_replace_settings
		WHERE id=1`)
	var setting BroadcastReplaceSetting
	err := row.Scan(&setting.Enabled, &setting.Text, &setting.ImageName, &setting.ImageData, &setting.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BroadcastReplaceSetting{}, nil
	}
	return setting, err
}

func (s *Store) SaveBroadcastReplaceSetting(ctx context.Context, setting BroadcastReplaceSetting) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO broadcast_replace_settings(id, enabled, text, image_name, image_data, updated_at)
		VALUES(1, $1, $2, $3, $4, $5)
		ON CONFLICT(id) DO UPDATE SET
			enabled=excluded.enabled,
			text=excluded.text,
			image_name=excluded.image_name,
			image_data=excluded.image_data,
			updated_at=excluded.updated_at`,
		setting.Enabled, setting.Text, setting.ImageName, setting.ImageData, setting.UpdatedAt)
	return err
}

func (s *Store) InsertRecord(ctx context.Context, r Record) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO records(
		chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt, subject_user_id, subject_name, actor_user_id, actor_name,
		source_message_id, bot_message_id, remark, created_at
	) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
	RETURNING id`,
		r.ChatID, r.DayKey, r.Kind, r.Currency, r.Amount, r.Rate, r.FeeRate, r.ResultUSDT, r.SubjectUserID,
		r.SubjectName, r.ActorUserID, r.ActorName, r.SourceMessageID, r.BotMessageID, r.Remark, r.CreatedAt).Scan(&id)
	return id, err
}

func (s *Store) SetRecordBotMessage(ctx context.Context, recordID, botMessageID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE records SET bot_message_id=$1 WHERE id=$2`, botMessageID, recordID)
	return err
}

func (s *Store) GetRecord(ctx context.Context, recordID int64) (Record, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
		subject_user_id, subject_name, actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
		FROM records
		WHERE id=$1`, recordID)
	record, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	return record, true, nil
}

const recordSelectColumns = `id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
	subject_user_id, subject_name, actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at`

func (s *Store) ListRecordsForDayPage(ctx context.Context, chatID int64, dayKey string, filter RecordFilter, beforeID, afterID int64, limit int) (RecordPage, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	where, args := recordPageWhere(chatID, dayKey, filter)
	ascending := afterID > 0
	if beforeID > 0 {
		args = append(args, beforeID)
		where += fmt.Sprintf(" AND id < $%d", len(args))
	} else if afterID > 0 {
		args = append(args, afterID)
		where += fmt.Sprintf(" AND id > $%d", len(args))
	}
	args = append(args, limit+1)
	order := "DESC"
	if ascending {
		order = "ASC"
	}
	rows, err := s.pool.Query(ctx, `SELECT `+recordSelectColumns+`
		FROM records WHERE `+where+`
		ORDER BY id `+order+`
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return RecordPage{}, err
	}
	defer rows.Close()
	records := make([]Record, 0, limit+1)
	for rows.Next() {
		record, scanErr := scanRecord(rows)
		if scanErr != nil {
			return RecordPage{}, scanErr
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return RecordPage{}, err
	}
	hasExtra := len(records) > limit
	if hasExtra {
		records = records[:limit]
	}
	if !ascending {
		reverseRecords(records)
	}
	return RecordPage{
		Records:  records,
		HasOlder: !ascending && hasExtra || ascending,
		HasNewer: ascending && hasExtra || beforeID > 0,
	}, nil
}

func (s *Store) WalkRecordsForDay(ctx context.Context, chatID int64, dayKey string, filter RecordFilter, batchSize int, visit func([]Record) error) error {
	if batchSize < 1 || batchSize > 1000 {
		batchSize = 500
	}
	var afterID int64
	for {
		where, args := recordPageWhere(chatID, dayKey, filter)
		args = append(args, afterID)
		where += fmt.Sprintf(" AND id > $%d", len(args))
		args = append(args, batchSize)
		rows, err := s.pool.Query(ctx, `SELECT `+recordSelectColumns+`
			FROM records WHERE `+where+`
			ORDER BY id ASC
			LIMIT $`+strconv.Itoa(len(args)), args...)
		if err != nil {
			return err
		}
		batch := make([]Record, 0, batchSize)
		for rows.Next() {
			record, scanErr := scanRecord(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			batch = append(batch, record)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		if err := visit(batch); err != nil {
			return err
		}
		afterID = batch[len(batch)-1].ID
		if len(batch) < batchSize {
			return nil
		}
	}
}

func recordPageWhere(chatID int64, dayKey string, filter RecordFilter) (string, []any) {
	where := "chat_id=$1 AND day_key=$2 AND deleted_at IS NULL"
	args := []any{chatID, dayKey}
	if kind := strings.TrimSpace(filter.Kind); kind != "" {
		args = append(args, kind)
		where += fmt.Sprintf(" AND kind=$%d", len(args))
	}
	query := strings.TrimSpace(filter.Query)
	if query == "" {
		return where, args
	}
	args = append(args, "%"+strings.ToLower(query)+"%")
	placeholder := "$" + strconv.Itoa(len(args))
	switch strings.ToLower(strings.TrimSpace(filter.Field)) {
	case "subject":
		where += " AND lower(COALESCE(NULLIF(subject_name,''), actor_name)) LIKE " + placeholder
	case "actor":
		where += " AND lower(actor_name) LIKE " + placeholder
	case "remark":
		where += " AND lower(remark) LIKE " + placeholder
	case "amount":
		where += " AND lower(concat_ws(' ', amount, rate, fee_rate, result_usdt, currency)) LIKE " + placeholder
	default:
		where += " AND lower(concat_ws(' ', kind, currency, amount, rate, fee_rate, result_usdt, subject_name, actor_name, remark, created_at::text)) LIKE " + placeholder
	}
	return where, args
}

func reverseRecords(records []Record) {
	for left, right := 0, len(records)-1; left < right; left, right = left+1, right-1 {
		records[left], records[right] = records[right], records[left]
	}
}

func (s *Store) GetBillSummaryForDay(ctx context.Context, chatID int64, dayKey string, recentLimit int) (BillSummaryData, error) {
	if recentLimit < 1 {
		recentLimit = 1
	}
	var data BillSummaryData
	row := s.pool.QueryRow(ctx, `SELECT
			COUNT(*) FILTER (WHERE kind='deposit'),
			COUNT(*) FILTER (WHERE kind='payout'),
			COALESCE(SUM(CASE WHEN kind='deposit' AND upper(currency)='CNY' THEN amount::numeric ELSE 0 END), 0)::TEXT,
			COALESCE(SUM(CASE WHEN kind='deposit' THEN result_usdt::numeric ELSE 0 END), 0)::TEXT,
			COALESCE(SUM(CASE WHEN kind='payout' THEN result_usdt::numeric ELSE 0 END), 0)::TEXT
		FROM records
		WHERE chat_id=$1 AND day_key=$2 AND deleted_at IS NULL`,
		chatID, dayKey)
	if err := row.Scan(
		&data.Summary.DepositCount,
		&data.Summary.PayoutCount,
		&data.Summary.TotalDepositCNY,
		&data.Summary.TotalDepositUSDT,
		&data.Summary.TotalPayoutUSDT,
	); err != nil {
		return BillSummaryData{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
			subject_user_id, subject_name, actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
		FROM (
			SELECT id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
				subject_user_id, subject_name, actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
			FROM records
			WHERE chat_id=$1 AND day_key=$2 AND deleted_at IS NULL AND kind='deposit'
			ORDER BY id DESC
			LIMIT $3
		) deposits
		UNION ALL
		SELECT id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
			subject_user_id, subject_name, actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
		FROM (
			SELECT id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
				subject_user_id, subject_name, actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
			FROM records
			WHERE chat_id=$1 AND day_key=$2 AND deleted_at IS NULL AND kind='payout'
			ORDER BY id DESC
			LIMIT $3
		) payouts
		ORDER BY id ASC`,
		chatID, dayKey, recentLimit)
	if err != nil {
		return BillSummaryData{}, err
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return BillSummaryData{}, err
		}
		data.Records = append(data.Records, record)
	}
	return data, rows.Err()
}

func (s *Store) ListBillDays(ctx context.Context, chatID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT day_key
		FROM records
		WHERE chat_id=$1 AND deleted_at IS NULL
		GROUP BY day_key
		ORDER BY day_key DESC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []string
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	return days, rows.Err()
}

func (s *Store) FindRecordByMessage(ctx context.Context, chatID, messageID int64) (Record, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
		subject_user_id, subject_name, actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
		FROM records
		WHERE chat_id=$1
		  AND deleted_at IS NULL
		  AND (source_message_id=$2 OR bot_message_id=$2)
		ORDER BY id DESC
		LIMIT 1`, chatID, messageID)
	if err != nil {
		return Record{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Record{}, false, rows.Err()
	}
	record, err := scanRecord(rows)
	return record, err == nil, err
}

func (s *Store) SoftDeleteRecord(ctx context.Context, chatID, recordID int64, now time.Time, kind string) (bool, error) {
	query := `UPDATE records SET deleted_at=$1 WHERE chat_id=$2 AND id=$3 AND deleted_at IS NULL`
	args := []any{now, chatID, recordID}
	if kind != "" {
		query += ` AND kind=$4`
		args = append(args, kind)
	}
	tag, err := s.pool.Exec(ctx, query, args...)
	return tag.RowsAffected() > 0, err
}

func (s *Store) CountRecordsForPeriod(ctx context.Context, chatID int64, dayKey string, startedAt time.Time) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM records
		WHERE chat_id=$1 AND day_key=$2 AND created_at >= $3 AND deleted_at IS NULL`, chatID, dayKey, startedAt).Scan(&count)
	return count, err
}

func (s *Store) SoftDeleteRecordsForPeriod(ctx context.Context, chatID int64, dayKey string, startedAt, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE records SET deleted_at=$1
		WHERE chat_id=$2 AND day_key=$3 AND created_at >= $4 AND deleted_at IS NULL`, now, chatID, dayKey, startedAt)
	return tag.RowsAffected(), err
}

func (s *Store) ListWatchTargets(ctx context.Context) ([]WatchTarget, error) {
	rows, err := s.pool.Query(ctx, `SELECT w.owner_user_id, w.address, w.label,
		w.watch_income, w.watch_expense, w.notify_trx, w.min_notify_amount,
		COALESCE(MAX(n.block_timestamp), 0)
		FROM address_watches w
		LEFT JOIN chain_notifications n ON n.owner_user_id = w.owner_user_id AND n.address = w.address
		WHERE w.active = TRUE
		GROUP BY w.owner_user_id, w.address, w.label, w.watch_income, w.watch_expense, w.notify_trx, w.min_notify_amount
		ORDER BY w.owner_user_id ASC, w.address ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []WatchTarget
	for rows.Next() {
		var t WatchTarget
		if err := rows.Scan(&t.OwnerUserID, &t.Address, &t.Label, &t.WatchIncome, &t.WatchExpense, &t.NotifyTRX, &t.MinNotifyAmount, &t.LatestTimestamp); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

func (s *Store) ListWatchTargetsForOwner(ctx context.Context, owner int64) ([]WatchTarget, error) {
	rows, err := s.pool.Query(ctx, `SELECT w.owner_user_id, w.address, w.label,
		w.watch_income, w.watch_expense, w.notify_trx, w.min_notify_amount,
		COALESCE(MAX(n.block_timestamp), 0)
		FROM address_watches w
		LEFT JOIN chain_notifications n ON n.owner_user_id = w.owner_user_id AND n.address = w.address
		WHERE w.active = TRUE AND w.owner_user_id=$1
		GROUP BY w.owner_user_id, w.address, w.label, w.watch_income, w.watch_expense, w.notify_trx, w.min_notify_amount
		ORDER BY w.updated_at DESC, w.address ASC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []WatchTarget
	for rows.Next() {
		var t WatchTarget
		if err := rows.Scan(&t.OwnerUserID, &t.Address, &t.Label, &t.WatchIncome, &t.WatchExpense, &t.NotifyTRX, &t.MinNotifyAmount, &t.LatestTimestamp); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

func (s *Store) CountActiveWatchTargetsForOwner(ctx context.Context, owner int64) (int, error) {
	row := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM address_watches WHERE owner_user_id=$1 AND active=TRUE`, owner)
	var count int
	err := row.Scan(&count)
	return count, err
}

func (s *Store) GetWatchSettings(ctx context.Context, owner int64) (WatchSettings, error) {
	settings := WatchSettings{
		OwnerUserID:     owner,
		WatchIncome:     true,
		WatchExpense:    true,
		NotifyTRX:       true,
		MinNotifyAmount: "0",
	}
	row := s.pool.QueryRow(ctx, `SELECT owner_user_id, watch_income, watch_expense, notify_trx, min_notify_amount, updated_at
		FROM address_watch_settings WHERE owner_user_id=$1`, owner)
	err := row.Scan(&settings.OwnerUserID, &settings.WatchIncome, &settings.WatchExpense, &settings.NotifyTRX, &settings.MinNotifyAmount, &settings.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return settings, nil
	}
	return settings, err
}

func (s *Store) SaveWatchSettings(ctx context.Context, settings WatchSettings, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO address_watch_settings(owner_user_id, watch_income, watch_expense, notify_trx, min_notify_amount, updated_at)
		VALUES($1, $2, $3, $4, $5, $6)
		ON CONFLICT(owner_user_id) DO UPDATE SET
			watch_income=excluded.watch_income,
			watch_expense=excluded.watch_expense,
			notify_trx=excluded.notify_trx,
			min_notify_amount=excluded.min_notify_amount,
			updated_at=excluded.updated_at`,
		settings.OwnerUserID, settings.WatchIncome, settings.WatchExpense, settings.NotifyTRX, settings.MinNotifyAmount, now)
	return err
}

func (s *Store) AddWatch(ctx context.Context, owner int64, address, label string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO address_watches(
			owner_user_id, address, label, watch_income, watch_expense, notify_trx, min_notify_amount, active, created_at, updated_at
		)
		SELECT $1, $2, $3,
			COALESCE(s.watch_income, TRUE),
			COALESCE(s.watch_expense, TRUE),
			COALESCE(s.notify_trx, TRUE),
			COALESCE(s.min_notify_amount, '0'),
			TRUE, $4, $5
		FROM (SELECT 1) seed
		LEFT JOIN address_watch_settings s ON s.owner_user_id=$1
		ON CONFLICT(owner_user_id, address) DO UPDATE SET label=excluded.label, active=TRUE, updated_at=excluded.updated_at`,
		owner, address, label, now, now)
	return err
}

func (s *Store) GetWatchTarget(ctx context.Context, owner int64, address string) (WatchTarget, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT w.owner_user_id, w.address, w.label,
		w.watch_income, w.watch_expense, w.notify_trx, w.min_notify_amount,
		COALESCE(MAX(n.block_timestamp), 0)
		FROM address_watches w
		LEFT JOIN chain_notifications n ON n.owner_user_id = w.owner_user_id AND n.address = w.address
		WHERE w.active = TRUE AND w.owner_user_id=$1 AND w.address=$2
		GROUP BY w.owner_user_id, w.address, w.label, w.watch_income, w.watch_expense, w.notify_trx, w.min_notify_amount`,
		owner, address)
	var target WatchTarget
	err := row.Scan(&target.OwnerUserID, &target.Address, &target.Label, &target.WatchIncome, &target.WatchExpense, &target.NotifyTRX, &target.MinNotifyAmount, &target.LatestTimestamp)
	if errors.Is(err, pgx.ErrNoRows) {
		return WatchTarget{}, false, nil
	}
	return target, err == nil, err
}

func (s *Store) UpdateWatchTarget(ctx context.Context, target WatchTarget, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE address_watches SET
			label=$3,
			watch_income=$4,
			watch_expense=$5,
			notify_trx=$6,
			min_notify_amount=$7,
			updated_at=$8
		WHERE owner_user_id=$1 AND address=$2 AND active=TRUE`,
		target.OwnerUserID,
		target.Address,
		strings.TrimSpace(target.Label),
		target.WatchIncome,
		target.WatchExpense,
		target.NotifyTRX,
		target.MinNotifyAmount,
		now,
	)
	return tag.RowsAffected() > 0, err
}

func (s *Store) RemoveWatch(ctx context.Context, owner int64, address string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE address_watches SET active=FALSE, updated_at=$1 WHERE owner_user_id=$2 AND address=$3 AND active=TRUE`,
		now, owner, address)
	return tag.RowsAffected() > 0, err
}

func (s *Store) RecordAddressValidation(ctx context.Context, chatID int64, address string, user User, now time.Time) (AddressValidation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AddressValidation{}, err
	}
	defer rollback(ctx, tx)
	var existing AddressValidation
	err = tx.QueryRow(ctx, `SELECT chat_id, address, verify_count, first_user_id, first_user_name,
		last_user_id, last_user_name, last_seen_at, created_at
		FROM address_validations
		WHERE chat_id=$1 AND address=$2
		FOR UPDATE`, chatID, address).Scan(
		&existing.ChatID,
		&existing.Address,
		&existing.VerifyCount,
		&existing.FirstUserID,
		&existing.FirstUserName,
		&existing.LastUserID,
		&existing.LastUserName,
		&existing.LastSeenAt,
		&existing.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		row := tx.QueryRow(ctx, `INSERT INTO address_validations(
			chat_id, address, verify_count, first_user_id, first_user_name,
			last_user_id, last_user_name, last_seen_at, created_at
		) VALUES($1, $2, 1, $3, $4, $3, $4, $5, $5)
		ON CONFLICT DO NOTHING
		RETURNING chat_id, address, verify_count, first_user_id, first_user_name,
			last_user_id, last_user_name, last_seen_at, created_at`,
			chatID, address, user.ID, user.DisplayName, now)
		var validation AddressValidation
		err := row.Scan(
			&validation.ChatID,
			&validation.Address,
			&validation.VerifyCount,
			&validation.FirstUserID,
			&validation.FirstUserName,
			&validation.LastUserID,
			&validation.LastUserName,
			&validation.LastSeenAt,
			&validation.CreatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return s.RecordAddressValidation(ctx, chatID, address, user, now)
		}
		if err != nil {
			return AddressValidation{}, err
		}
		return validation, tx.Commit(ctx)
	}
	if err != nil {
		return AddressValidation{}, err
	}
	row := tx.QueryRow(ctx, `UPDATE address_validations SET
			verify_count=verify_count + 1,
			last_user_id=$3,
			last_user_name=$4,
			last_seen_at=$5
		WHERE chat_id=$1 AND address=$2
		RETURNING chat_id, address, verify_count, first_user_id, first_user_name,
			last_user_id, last_user_name, last_seen_at, created_at`,
		chatID, address, user.ID, user.DisplayName, now)
	var validation AddressValidation
	if err := row.Scan(
		&validation.ChatID,
		&validation.Address,
		&validation.VerifyCount,
		&validation.FirstUserID,
		&validation.FirstUserName,
		&validation.LastUserID,
		&validation.LastUserName,
		&validation.LastSeenAt,
		&validation.CreatedAt,
	); err != nil {
		return AddressValidation{}, err
	}
	validation.PreviousUserID = existing.LastUserID
	validation.PreviousUserName = existing.LastUserName
	return validation, tx.Commit(ctx)
}

func (s *Store) RecordChainNotification(ctx context.Context, owner int64, address, txHash, direction string, blockTimestamp int64, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO chain_notifications(owner_user_id, address, tx_hash, event_id, direction, block_timestamp, created_at)
		VALUES($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`, owner, address, txHash, txHash, direction, blockTimestamp, now)
	return tag.RowsAffected() == 1, err
}

func (s *Store) RecordChainNotificationOutbox(ctx context.Context, owner int64, address, txHash, direction string, blockTimestamp int64, chatID int64, text, parseMode string, disablePreview bool, now time.Time) (bool, error) {
	return s.RecordChainNotificationOutboxEvent(ctx, owner, address, txHash, txHash, direction, blockTimestamp, chatID, text, parseMode, disablePreview, now)
}

func (s *Store) RecordChainNotificationOutboxEvent(ctx context.Context, owner int64, address, txHash, eventID, direction string, blockTimestamp int64, chatID int64, text, parseMode string, disablePreview bool, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	tag, err := tx.Exec(ctx, `INSERT INTO chain_notifications(owner_user_id, address, tx_hash, event_id, direction, block_timestamp, created_at)
		VALUES($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`, owner, address, txHash, eventID, direction, blockTimestamp, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	dedupeKey := fmt.Sprintf("chain:%d:%s:%s:%s", owner, address, eventID, direction)
	if _, err := tx.Exec(ctx, `INSERT INTO notification_outbox(
			kind, dedupe_key, chat_id, text, parse_mode, disable_preview, priority, status,
			attempts, next_attempt_at, created_at, updated_at
		) VALUES('chain', $1, $2, $3, $4, $5, 0, 'pending', 0, $6, $6, $6)
		ON CONFLICT(dedupe_key) DO NOTHING`,
		dedupeKey, chatID, text, parseMode, disablePreview, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) EnqueueNotification(ctx context.Context, item NotificationOutbox, now time.Time) (bool, error) {
	item.Kind = strings.TrimSpace(item.Kind)
	item.DedupeKey = strings.TrimSpace(item.DedupeKey)
	item.ParseMode = strings.TrimSpace(item.ParseMode)
	item.ReplyMarkupJSON = strings.TrimSpace(item.ReplyMarkupJSON)
	item.ReferenceKind = strings.TrimSpace(item.ReferenceKind)
	if item.Kind == "" {
		return false, errors.New("notification kind is empty")
	}
	if item.DedupeKey == "" {
		return false, errors.New("notification dedupe key is empty")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO notification_outbox(
			kind, dedupe_key, chat_id, text, parse_mode, disable_preview,
			reply_to_message_id, reply_markup_json, reference_kind, reference_id, priority,
			status, attempts, next_attempt_at, created_at, updated_at
		) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'pending', 0, $12, $12, $12)
		ON CONFLICT(dedupe_key) DO NOTHING`,
		item.Kind,
		item.DedupeKey,
		item.ChatID,
		item.Text,
		item.ParseMode,
		item.DisablePreview,
		item.ReplyToMessageID,
		item.ReplyMarkupJSON,
		item.ReferenceKind,
		item.ReferenceID,
		item.Priority,
		now,
	)
	return tag.RowsAffected() == 1, err
}

func (s *Store) ClaimDueNotifications(ctx context.Context, limit int, maxAttempts int, now time.Time) ([]NotificationOutbox, error) {
	if limit < 1 {
		limit = 1
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	staleBefore := now.Add(-2 * time.Minute)
	if _, err := s.pool.Exec(ctx, `UPDATE notification_outbox
		SET status='failed',
			last_error='notification send attempt expired after reaching retry limit',
			updated_at=$1
		WHERE status='sending'
			AND updated_at <= $2
			AND attempts >= $3`, now, staleBefore, maxAttempts); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `WITH next AS (
			SELECT id
			FROM notification_outbox
			WHERE ((status IN ('pending', 'failed') AND next_attempt_at <= $1)
				OR (status = 'sending' AND updated_at <= $3))
				AND attempts < $4
			ORDER BY priority ASC, next_attempt_at ASC, id ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notification_outbox n
		SET status='sending',
			attempts=n.attempts + 1,
			updated_at=$1
		FROM next
		WHERE n.id = next.id
		RETURNING n.id, n.kind, n.dedupe_key, n.chat_id, n.text, n.parse_mode,
			n.disable_preview, n.reply_to_message_id, n.reply_markup_json,
			n.reference_kind, n.reference_id, n.priority, n.status, n.attempts, n.next_attempt_at, n.last_error,
			n.created_at, n.updated_at, n.sent_at`, now, limit, staleBefore, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationOutbox
	for rows.Next() {
		var item NotificationOutbox
		if err := rows.Scan(
			&item.ID,
			&item.Kind,
			&item.DedupeKey,
			&item.ChatID,
			&item.Text,
			&item.ParseMode,
			&item.DisablePreview,
			&item.ReplyToMessageID,
			&item.ReplyMarkupJSON,
			&item.ReferenceKind,
			&item.ReferenceID,
			&item.Priority,
			&item.Status,
			&item.Attempts,
			&item.NextAttemptAt,
			&item.LastError,
			&item.CreatedAt,
			&item.UpdatedAt,
			&item.SentAt,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) NotificationOutboxStats(ctx context.Context, since time.Time) (NotificationOutboxStats, error) {
	if since.IsZero() {
		since = time.Now().Add(-72 * time.Hour)
	}
	var stats NotificationOutboxStats
	rows, err := s.pool.Query(ctx, `SELECT status, COUNT(*)
		FROM notification_outbox
		WHERE status IN ('pending', 'sending')
		   OR (status IN ('sent', 'failed') AND updated_at >= $1)
		GROUP BY status`, since)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return stats, err
		}
		switch status {
		case "pending":
			stats.Pending = count
		case "sending":
			stats.Sending = count
		case "sent":
			stats.Sent = count
		case "failed":
			stats.Failed = count
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, err
	}
	rows.Close()

	var oldest pgtype.Timestamptz
	row := s.pool.QueryRow(ctx, `SELECT MIN(created_at)
		FROM notification_outbox
		WHERE status='pending'`)
	if err := row.Scan(&oldest); err != nil {
		return stats, err
	}
	if oldest.Valid {
		value := oldest.Time
		stats.OldestPending = &value
	}

	row = s.pool.QueryRow(ctx, `SELECT COALESCE(last_error, '')
		FROM notification_outbox
		WHERE last_error <> ''
		  AND (status IN ('pending', 'sending') OR updated_at >= $1)
		ORDER BY updated_at DESC, id DESC
		LIMIT 1`, since)
	if err := row.Scan(&stats.LastError); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return stats, err
	}

	rows, err = s.pool.Query(ctx, `SELECT priority, status, COUNT(*)
		FROM notification_outbox
		WHERE status IN ('pending', 'sending')
		   OR (status IN ('sent', 'failed') AND updated_at >= $1)
		GROUP BY priority, status
		ORDER BY priority ASC, status ASC`, since)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var item NotificationPriorityCount
		if err := rows.Scan(&item.Priority, &item.Status, &item.Count); err != nil {
			rows.Close()
			return stats, err
		}
		stats.ByPriority = append(stats.ByPriority, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, err
	}
	rows.Close()

	rows, err = s.pool.Query(ctx, `SELECT CASE
			WHEN lower(last_error) LIKE '%retry_after%' OR last_error LIKE '% 429 %' THEN '429'
			WHEN last_error LIKE '% 500 %' OR last_error LIKE '% 502 %' OR last_error LIKE '% 503 %' OR last_error LIKE '% 504 %' THEN '5xx'
			WHEN lower(last_error) LIKE '%timeout%' OR lower(last_error) LIKE '%deadline exceeded%' OR lower(last_error) LIKE '%connection%' OR lower(last_error) LIKE '%network%' THEN 'network'
			WHEN lower(last_error) LIKE '%queue is full%' THEN 'queue_full'
			ELSE 'other'
		END AS failure_class,
		COUNT(*)
		FROM notification_outbox
		WHERE status='failed'
		  AND updated_at >= $1
		GROUP BY failure_class
		ORDER BY failure_class ASC`, since)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var item NotificationFailureClassCount
		if err := rows.Scan(&item.Class, &item.Count); err != nil {
			return stats, err
		}
		stats.FailureClasses = append(stats.FailureClasses, item)
	}
	return stats, rows.Err()
}

func (s *Store) CleanupNotificationOutbox(ctx context.Context, sentBefore time.Time, failedBefore time.Time) (NotificationOutboxCleanupStats, error) {
	var stats NotificationOutboxCleanupStats
	tag, err := s.pool.Exec(ctx, `DELETE FROM notification_outbox
		WHERE status='sent'
		  AND sent_at IS NOT NULL
		  AND sent_at < $1`, sentBefore)
	if err != nil {
		return stats, err
	}
	stats.SentDeleted = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM notification_outbox
		WHERE status='failed'
		  AND updated_at < $1`, failedBefore)
	if err != nil {
		return stats, err
	}
	stats.FailedDeleted = tag.RowsAffected()
	return stats, nil
}

func (s *Store) MarkNotificationSent(ctx context.Context, id int64, messageID int64, now time.Time) error {
	_, err := s.pool.Exec(ctx, `WITH marked AS (
			UPDATE notification_outbox
			SET status='sent', sent_at=$3, updated_at=$3, last_error=''
			WHERE id=$1
			RETURNING reference_kind, reference_id
		)
		UPDATE records r
		SET bot_message_id=$2
		FROM marked
		WHERE marked.reference_kind='ledger_record'
			AND marked.reference_id=r.id
			AND $2 > 0`, id, messageID, now)
	return err
}

func (s *Store) MarkNotificationFailed(ctx context.Context, id int64, message string, nextAttemptAt time.Time, now time.Time) error {
	message = strings.TrimSpace(message)
	if len(message) > 1000 {
		message = message[:1000]
	}
	_, err := s.pool.Exec(ctx, `UPDATE notification_outbox
		SET status='failed', last_error=$2, next_attempt_at=$3, updated_at=$4
		WHERE id=$1`, id, message, nextAttemptAt, now)
	return err
}

func (s *Store) UpsertChainWatcherBot(ctx context.Context, botID, secret string, now time.Time) error {
	botID = strings.TrimSpace(botID)
	secret = strings.TrimSpace(secret)
	if botID == "" {
		return errors.New("chain watcher bot id is empty")
	}
	if secret == "" {
		return errors.New("chain watcher bot secret is empty")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO chain_watcher_bots(bot_id, secret, status, created_at, updated_at)
		VALUES($1, $2, 'active', $3, $3)
		ON CONFLICT(bot_id) DO UPDATE SET secret=excluded.secret, status='active', updated_at=excluded.updated_at`,
		botID, secret, now)
	return err
}

func (s *Store) AuthenticateChainWatcherBot(ctx context.Context, botID, secret string) (bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT 1 FROM chain_watcher_bots
		WHERE bot_id=$1 AND secret=$2 AND status='active'
		LIMIT 1`, strings.TrimSpace(botID), strings.TrimSpace(secret))
	var one int
	err := row.Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) UpsertChainWatcherSubscription(ctx context.Context, sub ChainWatcherSubscription, now time.Time) error {
	sub.BotID = strings.TrimSpace(sub.BotID)
	sub.Address = strings.TrimSpace(sub.Address)
	sub.Label = strings.TrimSpace(sub.Label)
	sub.MinNotifyAmount = strings.TrimSpace(sub.MinNotifyAmount)
	if sub.BotID == "" {
		return errors.New("chain watcher subscription bot id is empty")
	}
	if sub.ChatID == 0 {
		sub.ChatID = sub.OwnerUserID
	}
	if sub.OwnerUserID == 0 {
		return errors.New("chain watcher subscription owner is empty")
	}
	if sub.Address == "" {
		return errors.New("chain watcher subscription address is empty")
	}
	if sub.MinNotifyAmount == "" {
		sub.MinNotifyAmount = "0"
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO chain_watcher_subscriptions(
			bot_id, chat_id, owner_user_id, address, label, watch_income, watch_expense, notify_trx, min_notify_amount,
			active, created_at, updated_at
		) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, TRUE, $10, $10)
		ON CONFLICT(bot_id, chat_id, owner_user_id, address) DO UPDATE SET
			label=excluded.label,
			watch_income=excluded.watch_income,
			watch_expense=excluded.watch_expense,
			notify_trx=excluded.notify_trx,
			min_notify_amount=excluded.min_notify_amount,
			active=TRUE,
			updated_at=excluded.updated_at`,
		sub.BotID, sub.ChatID, sub.OwnerUserID, sub.Address, sub.Label, sub.WatchIncome, sub.WatchExpense, sub.NotifyTRX, sub.MinNotifyAmount, now)
	return err
}

func (s *Store) RemoveChainWatcherSubscription(ctx context.Context, botID string, chatID int64, owner int64, address string, now time.Time) error {
	if chatID == 0 {
		chatID = owner
	}
	_, err := s.pool.Exec(ctx, `UPDATE chain_watcher_subscriptions
		SET active=FALSE, updated_at=$5
		WHERE bot_id=$1 AND chat_id=$2 AND owner_user_id=$3 AND address=$4`,
		strings.TrimSpace(botID), chatID, owner, strings.TrimSpace(address), now)
	return err
}

func (s *Store) ReplaceChainWatcherSubscriptions(ctx context.Context, botID string, subs []ChainWatcherSubscription, now time.Time) error {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return errors.New("chain watcher sync bot id is empty")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err := tx.Exec(ctx, `UPDATE chain_watcher_subscriptions SET active=FALSE, updated_at=$2 WHERE bot_id=$1`, botID, now); err != nil {
		return err
	}
	for _, sub := range subs {
		sub.BotID = botID
		if sub.ChatID == 0 {
			sub.ChatID = sub.OwnerUserID
		}
		sub.Address = strings.TrimSpace(sub.Address)
		sub.Label = strings.TrimSpace(sub.Label)
		sub.MinNotifyAmount = strings.TrimSpace(sub.MinNotifyAmount)
		if sub.OwnerUserID == 0 || sub.Address == "" {
			continue
		}
		if sub.MinNotifyAmount == "" {
			sub.MinNotifyAmount = "0"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO chain_watcher_subscriptions(
				bot_id, chat_id, owner_user_id, address, label, watch_income, watch_expense, notify_trx, min_notify_amount,
				active, created_at, updated_at
			) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, TRUE, $10, $10)
			ON CONFLICT(bot_id, chat_id, owner_user_id, address) DO UPDATE SET
				label=excluded.label,
				watch_income=excluded.watch_income,
				watch_expense=excluded.watch_expense,
				notify_trx=excluded.notify_trx,
				min_notify_amount=excluded.min_notify_amount,
				active=TRUE,
				updated_at=excluded.updated_at`,
			sub.BotID, sub.ChatID, sub.OwnerUserID, sub.Address, sub.Label, sub.WatchIncome, sub.WatchExpense, sub.NotifyTRX, sub.MinNotifyAmount, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ListChainWatcherSubscriptions(ctx context.Context) ([]ChainWatcherSubscription, error) {
	rows, err := s.pool.Query(ctx, `SELECT bot_id, chat_id, owner_user_id, address, label,
		watch_income, watch_expense, notify_trx, min_notify_amount, active, updated_at
		FROM chain_watcher_subscriptions
		WHERE active=TRUE
		ORDER BY address, bot_id, owner_user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChainWatcherSubscription
	for rows.Next() {
		var sub ChainWatcherSubscription
		if err := rows.Scan(&sub.BotID, &sub.ChatID, &sub.OwnerUserID, &sub.Address, &sub.Label, &sub.WatchIncome, &sub.WatchExpense, &sub.NotifyTRX, &sub.MinNotifyAmount, &sub.Active, &sub.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *Store) RecordChainWatcherMatches(ctx context.Context, event ChainWatcherEvent, deliveries []ChainWatcherMatchedEvent, now time.Time) (int, error) {
	event.EventID = strings.TrimSpace(event.EventID)
	event.TxHash = strings.TrimSpace(event.TxHash)
	if event.EventID == "" {
		return 0, errors.New("chain watcher event id is empty")
	}
	if event.TxHash == "" {
		return 0, errors.New("chain watcher tx hash is empty")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(ctx, tx)
	eventTag, err := tx.Exec(ctx, `INSERT INTO chain_watcher_events(
			event_id, tx_hash, contract, from_address, to_address, value, token_symbol, token_address,
			token_decimals, block_timestamp, confirmed, source, event_index, created_at
		) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT(event_id) DO NOTHING`,
		event.EventID, event.TxHash, event.Contract, event.From, event.To, event.Value, event.TokenSymbol, event.TokenAddress,
		event.TokenDecimals, event.BlockTimestamp, event.Confirmed, event.Source, event.EventIndex, now)
	if err != nil {
		return 0, err
	}
	if eventTag.RowsAffected() == 0 {
		return 0, tx.Commit(ctx)
	}
	inserted := 0
	for _, d := range deliveries {
		d.DeliveryID = strings.TrimSpace(d.DeliveryID)
		if d.ChatID == 0 {
			d.ChatID = d.OwnerUserID
		}
		if d.DeliveryID == "" || d.BotID == "" || d.ChatID == 0 || d.OwnerUserID == 0 || d.WatchAddress == "" {
			continue
		}
		tag, err := tx.Exec(ctx, `INSERT INTO chain_watcher_matched_events(
				delivery_id, event_id, bot_id, chat_id, owner_user_id, watch_address, label, direction,
				tx_hash, from_address, to_address, value, token_symbol, token_address, token_decimals,
				block_timestamp, confirmed, status, attempts, next_attempt_at, created_at, updated_at
			) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, 'pending', 0, $18, $18, $18)
			ON CONFLICT(delivery_id) DO NOTHING`,
			d.DeliveryID, event.EventID, d.BotID, d.ChatID, d.OwnerUserID, d.WatchAddress, d.Label, d.Direction,
			event.TxHash, event.From, event.To, event.Value, event.TokenSymbol, event.TokenAddress, event.TokenDecimals,
			event.BlockTimestamp, event.Confirmed, now)
		if err != nil {
			return 0, err
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, tx.Commit(ctx)
}

func (s *Store) ClaimChainWatcherMatchedEvents(ctx context.Context, botID string, limit int, lease time.Duration, maxAge time.Duration, now time.Time) ([]ChainWatcherMatchedEvent, error) {
	if limit < 1 {
		limit = 1
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	staleBefore := now.Add(-lease)
	keepAfter := now.Add(-maxAge)
	rows, err := s.pool.Query(ctx, `WITH next AS (
			SELECT delivery_id
			FROM chain_watcher_matched_events
			WHERE bot_id=$1
				AND (
					status = 'pending'
					OR (status = 'delivering' AND updated_at <= $4)
				)
				AND next_attempt_at <= $2
				AND created_at >= $5
			ORDER BY created_at ASC, delivery_id ASC
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE chain_watcher_matched_events m
		SET status='delivering',
			attempts=m.attempts + 1,
			updated_at=$2
		FROM next
		WHERE m.delivery_id=next.delivery_id
		RETURNING m.delivery_id, m.event_id, m.bot_id, m.chat_id, m.owner_user_id, m.watch_address, m.label,
			m.direction, m.tx_hash, m.from_address, m.to_address, m.value, m.token_symbol,
			m.token_address, m.token_decimals, m.block_timestamp, m.confirmed, m.status, m.attempts,
			m.created_at, m.updated_at, m.delivered_at`,
		strings.TrimSpace(botID), now, limit, staleBefore, keepAfter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChainWatcherMatchedEvent
	for rows.Next() {
		var item ChainWatcherMatchedEvent
		if err := rows.Scan(&item.DeliveryID, &item.EventID, &item.BotID, &item.ChatID, &item.OwnerUserID, &item.WatchAddress, &item.Label,
			&item.Direction, &item.TxHash, &item.From, &item.To, &item.Value, &item.TokenSymbol,
			&item.TokenAddress, &item.TokenDecimals, &item.BlockTimestamp, &item.Confirmed, &item.Status, &item.Attempts,
			&item.CreatedAt, &item.UpdatedAt, &item.DeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) AckChainWatcherMatchedEvents(ctx context.Context, botID string, deliveryIDs []string, now time.Time) error {
	if len(deliveryIDs) == 0 {
		return nil
	}
	clean := make([]string, 0, len(deliveryIDs))
	for _, id := range deliveryIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE chain_watcher_matched_events
		SET status='delivered', delivered_at=$3, updated_at=$3
		WHERE bot_id=$1 AND delivery_id = ANY($2)`,
		strings.TrimSpace(botID), clean, now)
	return err
}

func (s *Store) ChainWatcherDeliveryStats(ctx context.Context, maxAge time.Duration, now time.Time) (ChainWatcherDeliveryStats, error) {
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	keepAfter := now.Add(-maxAge)
	var stats ChainWatcherDeliveryStats
	var oldest pgtype.Timestamptz
	err := s.pool.QueryRow(ctx, `SELECT
			COUNT(*) FILTER (WHERE status='pending'),
			COUNT(*) FILTER (WHERE status='delivering'),
			MIN(created_at) FILTER (WHERE status IN ('pending', 'delivering'))
		FROM chain_watcher_matched_events
		WHERE created_at >= $1`, keepAfter).Scan(&stats.PendingCount, &stats.DeliveringCount, &oldest)
	if err != nil {
		return stats, err
	}
	if oldest.Valid {
		stats.OldestPendingAt = &oldest.Time
		stats.OldestPendingAgeMS = now.Sub(oldest.Time).Milliseconds()
		if stats.OldestPendingAgeMS < 0 {
			stats.OldestPendingAgeMS = 0
		}
	}
	return stats, nil
}

func (s *Store) LoadTronscanKeyUsage(ctx context.Context, fingerprints []string, budgetDay string) (map[string]tron.KeyUsageRecord, error) {
	records := make(map[string]tron.KeyUsageRecord)
	if len(fingerprints) == 0 {
		return records, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT fingerprint, budget_day::text, request_count,
		main_request_count, comp_request_count, other_request_count, failover_count,
		rate_limit_count, auth_error_count, last_http_status, last_429_at, cooldown_until, disabled_until
		FROM chain_watcher_key_usage
		WHERE budget_day=$1::date AND fingerprint = ANY($2)`, budgetDay, fingerprints)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanTronscanKeyUsage(rows)
		if err != nil {
			return nil, err
		}
		records[record.Fingerprint] = record
	}
	return records, rows.Err()
}

func (s *Store) ListTronscanAPIKeys(ctx context.Context) ([]tron.KeyRegistryRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT fingerprint, api_key_ciphertext, enabled, health, reason,
		consecutive_failures, consecutive_auth_failures, consecutive_probe_successes,
		cooldown_until, next_probe_at, last_used_at, last_success_at, last_failure_at,
		last_error_class FROM chain_watcher_api_keys ORDER BY created_at, fingerprint`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []tron.KeyRegistryRecord
	for rows.Next() {
		record, err := scanTronscanAPIKey(rows, s.keyCipher)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) UpsertTronscanAPIKey(ctx context.Context, fingerprint, apiKey string, enabled bool, now time.Time) error {
	ciphertext, err := s.keyCipher.encrypt(apiKey)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('chain_watcher_api_keys'))`); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chain_watcher_api_keys WHERE fingerprint=$1)`, fingerprint).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM chain_watcher_api_keys`).Scan(&count); err != nil {
			return err
		}
		if count >= tron.MaxConfiguredKeys {
			return fmt.Errorf("Tronscan API key registry supports at most %d keys; delete one before adding another", tron.MaxConfiguredKeys)
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO chain_watcher_api_keys(
		fingerprint, api_key, api_key_ciphertext, enabled, health, reason, next_probe_at, created_at, updated_at
	) VALUES($1,'',$2,$3,'suspect','new_or_updated',$4,$4,$4)
	ON CONFLICT(fingerprint) DO UPDATE SET api_key='', api_key_ciphertext=excluded.api_key_ciphertext, enabled=excluded.enabled,
		health='suspect', reason='new_or_updated', consecutive_failures=0,
		consecutive_auth_failures=0, consecutive_probe_successes=0,
		cooldown_until=NULL, next_probe_at=excluded.next_probe_at,
		last_error_class='', updated_at=excluded.updated_at`, fingerprint, ciphertext, enabled, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DeleteTronscanAPIKey(ctx context.Context, fingerprint string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM chain_watcher_api_keys WHERE fingerprint=$1`, fingerprint)
	return err
}

func (s *Store) UpdateTronscanAPIKeyState(ctx context.Context, record tron.KeyRegistryRecord, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE chain_watcher_api_keys SET enabled=$2, health=$3,
		reason=$4, consecutive_failures=$5, consecutive_auth_failures=$6,
		consecutive_probe_successes=$7, cooldown_until=$8, next_probe_at=$9,
		last_used_at=$10, last_success_at=$11, last_failure_at=$12,
		last_error_class=$13, updated_at=$14 WHERE fingerprint=$1`,
		record.Fingerprint, record.Enabled, record.Health, record.Reason,
		record.ConsecutiveFailures, record.ConsecutiveAuthFailures, record.ConsecutiveProbeSuccesses,
		nullableTime(record.CooldownUntil), nullableTime(record.NextProbeAt), nullableTime(record.LastUsedAt),
		nullableTime(record.LastSuccessAt), nullableTime(record.LastFailureAt), record.LastErrorClass, now)
	return err
}

func scanTronscanAPIKey(scanner recordScanner, cipher *keyCipher) (tron.KeyRegistryRecord, error) {
	var record tron.KeyRegistryRecord
	var ciphertext []byte
	var cooldown, nextProbe, lastUsed, lastSuccess, lastFailure pgtype.Timestamptz
	err := scanner.Scan(&record.Fingerprint, &ciphertext, &record.Enabled, &record.Health,
		&record.Reason, &record.ConsecutiveFailures, &record.ConsecutiveAuthFailures,
		&record.ConsecutiveProbeSuccesses, &cooldown, &nextProbe, &lastUsed,
		&lastSuccess, &lastFailure, &record.LastErrorClass)
	if err != nil {
		return record, err
	}
	record.APIKey, err = cipher.decrypt(ciphertext)
	if err != nil {
		return record, fmt.Errorf("decrypt Tronscan key %s: %w", record.Fingerprint, err)
	}
	if cooldown.Valid {
		record.CooldownUntil = cooldown.Time
	}
	if nextProbe.Valid {
		record.NextProbeAt = nextProbe.Time
	}
	if lastUsed.Valid {
		record.LastUsedAt = lastUsed.Time
	}
	if lastSuccess.Valid {
		record.LastSuccessAt = lastSuccess.Time
	}
	if lastFailure.Valid {
		record.LastFailureAt = lastFailure.Time
	}
	return record, err
}

func (s *Store) migrateTronscanKeyEncryption(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT fingerprint, api_key FROM chain_watcher_api_keys
		WHERE api_key <> '' AND api_key_ciphertext IS NULL`)
	if err != nil {
		return err
	}
	type legacyKey struct{ fingerprint, key string }
	var legacy []legacyKey
	for rows.Next() {
		var item legacyKey
		if err := rows.Scan(&item.fingerprint, &item.key); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range legacy {
		ciphertext, err := s.keyCipher.encrypt(item.key)
		if err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `UPDATE chain_watcher_api_keys SET api_key='', api_key_ciphertext=$2 WHERE fingerprint=$1`, item.fingerprint, ciphertext); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetChainWatcherWatermark(ctx context.Context) (ChainWatcherWatermark, error) {
	var watermark ChainWatcherWatermark
	err := s.pool.QueryRow(ctx, `SELECT global_watermark_timestamp, global_watermark_tx_hash,
		watermark_source, updated_at FROM chain_watcher_runtime_state WHERE id=1`).Scan(
		&watermark.Timestamp, &watermark.TxHash, &watermark.Source, &watermark.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return watermark, nil
	}
	return watermark, err
}

func (s *Store) AdvanceChainWatcherWatermark(ctx context.Context, timestamp int64, txHash, source string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO chain_watcher_runtime_state(
		id, global_watermark_timestamp, global_watermark_tx_hash, watermark_source, updated_at
	) VALUES(1, $1, $2, $3, $4)
	ON CONFLICT(id) DO UPDATE SET
		global_watermark_timestamp=excluded.global_watermark_timestamp,
		global_watermark_tx_hash=excluded.global_watermark_tx_hash,
		watermark_source=excluded.watermark_source,
		updated_at=excluded.updated_at
	WHERE excluded.global_watermark_timestamp >= chain_watcher_runtime_state.global_watermark_timestamp`,
		timestamp, txHash, source, now)
	return err
}

func (s *Store) GetChainWatcherRealtimeWatermark(ctx context.Context) (ChainWatcherWatermark, error) {
	var watermark ChainWatcherWatermark
	var updated pgtype.Timestamptz
	err := s.pool.QueryRow(ctx, `SELECT realtime_watermark_timestamp, realtime_watermark_tx_hash, realtime_updated_at
		FROM chain_watcher_runtime_state WHERE id=1`).Scan(&watermark.Timestamp, &watermark.TxHash, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return watermark, nil
	}
	if updated.Valid {
		watermark.UpdatedAt = updated.Time
	}
	watermark.Source = "realtime"
	return watermark, err
}

func (s *Store) AdvanceChainWatcherRealtimeWatermark(ctx context.Context, timestamp int64, txHash string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO chain_watcher_runtime_state(
		id, global_watermark_timestamp, global_watermark_tx_hash, watermark_source,
		realtime_watermark_timestamp, realtime_watermark_tx_hash, realtime_updated_at, updated_at
	) VALUES(1, 0, '', '', $1, $2, $3, $3)
	ON CONFLICT(id) DO UPDATE SET
		realtime_watermark_timestamp=excluded.realtime_watermark_timestamp,
		realtime_watermark_tx_hash=excluded.realtime_watermark_tx_hash,
		realtime_updated_at=excluded.realtime_updated_at,
		updated_at=excluded.updated_at
	WHERE excluded.realtime_watermark_timestamp >= chain_watcher_runtime_state.realtime_watermark_timestamp`,
		timestamp, txHash, now)
	return err
}

func (s *Store) GetChainWatcherFallbackHead(ctx context.Context) (ChainWatcherWatermark, error) {
	var head ChainWatcherWatermark
	var updated pgtype.Timestamptz
	err := s.pool.QueryRow(ctx, `SELECT fallback_head_timestamp, fallback_anchor_event_id, fallback_head_updated_at
		FROM chain_watcher_runtime_state WHERE id=1`).Scan(&head.Timestamp, &head.TxHash, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return head, nil
	}
	if updated.Valid {
		head.UpdatedAt = updated.Time
	}
	head.Source = "fallback"
	return head, err
}

func (s *Store) AdvanceChainWatcherFallbackHead(ctx context.Context, timestamp int64, anchorID string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO chain_watcher_runtime_state(
		id, global_watermark_timestamp, global_watermark_tx_hash, watermark_source,
		fallback_head_timestamp, fallback_anchor_event_id, fallback_head_updated_at, updated_at
	) VALUES(1,0,'','',$1,$2,$3,$3)
	ON CONFLICT(id) DO UPDATE SET fallback_head_timestamp=excluded.fallback_head_timestamp,
		fallback_anchor_event_id=excluded.fallback_anchor_event_id,
		fallback_head_updated_at=excluded.fallback_head_updated_at, updated_at=excluded.updated_at
	WHERE excluded.fallback_head_timestamp >= chain_watcher_runtime_state.fallback_head_timestamp`, timestamp, anchorID, now)
	return err
}

func (s *Store) GetChainWatcherCatchupState(ctx context.Context) (ChainWatcherCatchupState, error) {
	var state ChainWatcherCatchupState
	var updated pgtype.Timestamptz
	err := s.pool.QueryRow(ctx, `SELECT catchup_required, catchup_reason, catchup_updated_at
		FROM chain_watcher_runtime_state WHERE id=1`).Scan(&state.Required, &state.Reason, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, nil
	}
	if updated.Valid {
		state.UpdatedAt = updated.Time
	}
	return state, err
}

func (s *Store) MarkChainWatcherCatchupRequired(ctx context.Context, reason string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO chain_watcher_runtime_state(
		id, global_watermark_timestamp, global_watermark_tx_hash, watermark_source,
		catchup_required, catchup_reason, catchup_updated_at, updated_at
	) VALUES(1,0,'','',TRUE,$1,$2,$2)
	ON CONFLICT(id) DO UPDATE SET catchup_required=TRUE, catchup_reason=excluded.catchup_reason,
		catchup_updated_at=excluded.catchup_updated_at, updated_at=excluded.updated_at`, reason, now)
	return err
}

func (s *Store) ClearChainWatcherCatchupRequired(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE chain_watcher_runtime_state SET catchup_required=FALSE,
		catchup_reason='', catchup_updated_at=$1, updated_at=$1 WHERE id=1`, now)
	return err
}

func (s *Store) AcquireChainWatcherFallbackLease(ctx context.Context, leaseName, holderID, mode string, ttl time.Duration, now time.Time) (ChainWatcherFallbackLease, bool, error) {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	leaseUntil := now.Add(ttl)
	row := s.pool.QueryRow(ctx, `INSERT INTO chain_watcher_fallback_lease(
		lease_name, holder_id, lease_until, mode, started_at, updated_at
	) VALUES($1, $2, $3, $4, $5, $5)
	ON CONFLICT(lease_name) DO UPDATE SET
		holder_id=excluded.holder_id,
		lease_until=excluded.lease_until,
		mode=excluded.mode,
		started_at=COALESCE(chain_watcher_fallback_lease.started_at, excluded.started_at),
		fallback_requests=CASE WHEN chain_watcher_fallback_lease.updated_at < $5 - interval '72 hours' THEN 0 ELSE chain_watcher_fallback_lease.fallback_requests END,
		fallback_429=CASE WHEN chain_watcher_fallback_lease.updated_at < $5 - interval '72 hours' THEN 0 ELSE chain_watcher_fallback_lease.fallback_429 END,
		catchup_pages=CASE WHEN chain_watcher_fallback_lease.updated_at < $5 - interval '72 hours' THEN 0 ELSE chain_watcher_fallback_lease.catchup_pages END,
		catchup_budget_used=CASE WHEN chain_watcher_fallback_lease.updated_at < $5 - interval '72 hours' THEN 0 ELSE chain_watcher_fallback_lease.catchup_budget_used END,
		updated_at=excluded.updated_at
	WHERE chain_watcher_fallback_lease.lease_until <= $5
	   OR chain_watcher_fallback_lease.holder_id=$2
	RETURNING lease_name, holder_id, lease_until, mode, started_at, last_watcher_success,
		fallback_requests, fallback_429, catchup_from, catchup_to, catchup_pages,
		catchup_budget_used, updated_at`, leaseName, holderID, leaseUntil, mode, now)
	lease, err := scanChainWatcherFallbackLease(row)
	if err == nil {
		return lease, lease.HolderID == holderID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return lease, false, err
	}
	lease, err = s.GetChainWatcherFallbackLease(ctx, leaseName)
	return lease, false, err
}

func (s *Store) UpdateChainWatcherFallbackLease(ctx context.Context, leaseName, holderID, mode string, lastWatcherSuccess time.Time, requestDelta, rateLimitDelta int64, catchupFrom, catchupTo int64, catchupPages, catchupBudgetUsed int64, ttl time.Duration, now time.Time) error {
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	_, err := s.pool.Exec(ctx, `UPDATE chain_watcher_fallback_lease SET
		lease_until=$3, mode=$4,
		last_watcher_success=COALESCE($5, last_watcher_success),
		fallback_requests=fallback_requests+$6,
		fallback_429=fallback_429+$7,
		catchup_from=$8, catchup_to=$9,
		catchup_pages=catchup_pages+$10,
		catchup_budget_used=catchup_budget_used+$11,
		updated_at=$12
	WHERE lease_name=$1 AND holder_id=$2`, leaseName, holderID, now.Add(ttl), mode,
		nullableTime(lastWatcherSuccess), requestDelta, rateLimitDelta, catchupFrom, catchupTo,
		catchupPages, catchupBudgetUsed, now)
	return err
}

func (s *Store) ReleaseChainWatcherFallbackLease(ctx context.Context, leaseName, holderID, mode string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE chain_watcher_fallback_lease
		SET lease_until=$3, mode=$4, holder_id='', updated_at=$3
		WHERE lease_name=$1 AND holder_id=$2`, leaseName, holderID, now, mode)
	return err
}

func (s *Store) GetChainWatcherFallbackLease(ctx context.Context, leaseName string) (ChainWatcherFallbackLease, error) {
	row := s.pool.QueryRow(ctx, `SELECT lease_name, holder_id, lease_until, mode, started_at,
		last_watcher_success, fallback_requests, fallback_429, catchup_from, catchup_to,
		catchup_pages, catchup_budget_used, updated_at
		FROM chain_watcher_fallback_lease WHERE lease_name=$1`, leaseName)
	lease, err := scanChainWatcherFallbackLease(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChainWatcherFallbackLease{LeaseName: leaseName, Mode: "PRIMARY"}, nil
	}
	return lease, err
}

func scanChainWatcherFallbackLease(scanner recordScanner) (ChainWatcherFallbackLease, error) {
	var lease ChainWatcherFallbackLease
	var startedAt, lastSuccess pgtype.Timestamptz
	err := scanner.Scan(&lease.LeaseName, &lease.HolderID, &lease.LeaseUntil, &lease.Mode,
		&startedAt, &lastSuccess, &lease.FallbackRequests, &lease.Fallback429,
		&lease.CatchupFrom, &lease.CatchupTo, &lease.CatchupPages, &lease.CatchupBudgetUsed, &lease.UpdatedAt)
	if startedAt.Valid {
		lease.StartedAt = &startedAt.Time
	}
	if lastSuccess.Valid {
		lease.LastWatcherSuccess = &lastSuccess.Time
	}
	return lease, err
}

func (s *Store) ReserveTronscanKeyRequest(ctx context.Context, fingerprint, budgetDay string, source tron.RequestSource, failover bool, dailyLimit int, now time.Time) (tron.KeyUsageRecord, bool, error) {
	mainInc := source == tron.RequestSourceMain
	compInc := source == tron.RequestSourceCompensation
	otherInc := !mainInc && !compInc
	row := s.pool.QueryRow(ctx, `INSERT INTO chain_watcher_key_usage(
		fingerprint, budget_day, request_count, main_request_count, comp_request_count,
		other_request_count, failover_count, updated_at
	) VALUES($1, $2::date, 1, CASE WHEN $3 THEN 1 ELSE 0 END,
		CASE WHEN $4 THEN 1 ELSE 0 END, CASE WHEN $5 THEN 1 ELSE 0 END,
		CASE WHEN $6 THEN 1 ELSE 0 END, $7)
	ON CONFLICT(fingerprint, budget_day) DO UPDATE SET
		request_count=chain_watcher_key_usage.request_count+1,
		main_request_count=chain_watcher_key_usage.main_request_count+CASE WHEN $3 THEN 1 ELSE 0 END,
		comp_request_count=chain_watcher_key_usage.comp_request_count+CASE WHEN $4 THEN 1 ELSE 0 END,
		other_request_count=chain_watcher_key_usage.other_request_count+CASE WHEN $5 THEN 1 ELSE 0 END,
		failover_count=chain_watcher_key_usage.failover_count+CASE WHEN $6 THEN 1 ELSE 0 END,
		updated_at=$7
	WHERE $8 <= 0 OR chain_watcher_key_usage.request_count < $8
	RETURNING fingerprint, budget_day::text, request_count, main_request_count,
		comp_request_count, other_request_count, failover_count, rate_limit_count,
		auth_error_count, last_http_status, last_429_at, cooldown_until, disabled_until`,
		fingerprint, budgetDay, mainInc, compInc, otherInc, failover, now, dailyLimit)
	record, err := scanTronscanKeyUsage(row)
	if err == nil {
		return record, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return tron.KeyUsageRecord{}, false, err
	}
	row = s.pool.QueryRow(ctx, `SELECT fingerprint, budget_day::text, request_count,
		main_request_count, comp_request_count, other_request_count, failover_count,
		rate_limit_count, auth_error_count, last_http_status, last_429_at, cooldown_until, disabled_until
		FROM chain_watcher_key_usage WHERE fingerprint=$1 AND budget_day=$2::date`, fingerprint, budgetDay)
	record, err = scanTronscanKeyUsage(row)
	return record, false, err
}

func (s *Store) RecordTronscanKeyResult(ctx context.Context, fingerprint, budgetDay string, status int, last429At, cooldownUntil, disabledUntil time.Time) (tron.KeyUsageRecord, error) {
	row := s.pool.QueryRow(ctx, `UPDATE chain_watcher_key_usage SET
		rate_limit_count=rate_limit_count+CASE WHEN $3=429 THEN 1 ELSE 0 END,
		auth_error_count=auth_error_count+CASE WHEN $3 IN (401,403) THEN 1 ELSE 0 END,
		last_http_status=$3,
		last_429_at=CASE WHEN $3=429 THEN $4 ELSE last_429_at END,
		cooldown_until=$5,
		disabled_until=$6,
		updated_at=$7
	WHERE fingerprint=$1 AND budget_day=$2::date
	RETURNING fingerprint, budget_day::text, request_count, main_request_count,
		comp_request_count, other_request_count, failover_count, rate_limit_count,
		auth_error_count, last_http_status, last_429_at, cooldown_until, disabled_until`,
		fingerprint, budgetDay, status, nullableTime(last429At), nullableTime(cooldownUntil), nullableTime(disabledUntil), time.Now())
	return scanTronscanKeyUsage(row)
}

func scanTronscanKeyUsage(scanner recordScanner) (tron.KeyUsageRecord, error) {
	var record tron.KeyUsageRecord
	var last429At, cooldownUntil, disabledUntil pgtype.Timestamptz
	err := scanner.Scan(
		&record.Fingerprint, &record.BudgetDay, &record.RequestCount,
		&record.MainRequestCount, &record.CompRequestCount, &record.OtherRequestCount,
		&record.FailoverCount, &record.RateLimitCount, &record.AuthErrorCount,
		&record.LastHTTPStatus, &last429At, &cooldownUntil, &disabledUntil,
	)
	if err != nil {
		return record, err
	}
	if last429At.Valid {
		record.Last429At = last429At.Time
	}
	if cooldownUntil.Valid {
		record.CooldownUntil = cooldownUntil.Time
	}
	if disabledUntil.Valid {
		record.DisabledUntil = disabledUntil.Time
	}
	return record, nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (s *Store) CleanupChainWatcherRetention(ctx context.Context, maxAge time.Duration, now time.Time) (ChainWatcherCleanupStats, error) {
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	var stats ChainWatcherCleanupStats
	cutoff := now.Add(-maxAge)
	tag, err := s.pool.Exec(ctx, `DELETE FROM chain_watcher_matched_events WHERE created_at < $1`, cutoff)
	if err != nil {
		return stats, err
	}
	stats.MatchedDeleted = tag.RowsAffected()
	tag, err = s.pool.Exec(ctx, `DELETE FROM chain_watcher_events WHERE created_at < $1`, cutoff)
	if err != nil {
		return stats, err
	}
	stats.EventsDeleted = tag.RowsAffected()
	if _, err := s.pool.Exec(ctx, `DELETE FROM chain_watcher_key_usage WHERE updated_at < $1`, now.Add(-72*time.Hour)); err != nil {
		return stats, err
	}
	return stats, nil
}

func rollback(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(ctx)
}

type recordScanner interface {
	Scan(dest ...any) error
}

func scanRecord(scanner recordScanner) (Record, error) {
	var record Record
	err := scanner.Scan(
		&record.ID,
		&record.ChatID,
		&record.DayKey,
		&record.Kind,
		&record.Currency,
		&record.Amount,
		&record.Rate,
		&record.FeeRate,
		&record.ResultUSDT,
		&record.SubjectUserID,
		&record.SubjectName,
		&record.ActorUserID,
		&record.ActorName,
		&record.SourceMessageID,
		&record.BotMessageID,
		&record.Remark,
		&record.CreatedAt,
		&record.DeletedAt,
	)
	return record, err
}

func uniqueInt64(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	unique := make([]int64, 0, len(values))
	for _, value := range values {
		if value == 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
}
