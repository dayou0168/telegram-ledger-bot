package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
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
	if err := store.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
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
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_operators_status
			ON broadcast_operators(status, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_operators_list
			ON broadcast_operators(status, updated_at DESC, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_broadcast_operators_created_by
			ON broadcast_operators(created_by, status, created_at, user_id)`,
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
		`CREATE INDEX IF NOT EXISTS idx_records_chat_active_id
			ON records(chat_id, id DESC)
			WHERE deleted_at IS NULL`,
		`ALTER TABLE records ADD COLUMN IF NOT EXISTS subject_user_id BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE records ADD COLUMN IF NOT EXISTS subject_name TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_records_chat_kind_active
			ON records(chat_id, kind, id DESC)
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
			created_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chain_watcher_events_tx
			ON chain_watcher_events(tx_hash, block_timestamp DESC)`,
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
	row := s.pool.QueryRow(ctx, `SELECT chat_id, title, active, active_day_key, business_open, owner_user_id,
		deposit_rate, payout_rate, deposit_exchange_rate, payout_exchange_rate,
		exchange_rate_source, exchange_rate_rank, exchange_rate_offset, fee_rate,
		cutoff_hour, all_members_can_record, created_at, updated_at
		FROM groups WHERE chat_id = $1`, chatID)
	return scanGroup(row)
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.pool.Query(ctx, `SELECT chat_id, title, active, active_day_key, business_open, owner_user_id,
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
	err := scanner.Scan(&g.ChatID, &g.Title, &g.Active, &g.ActiveDayKey, &g.BusinessOpen, &g.OwnerUserID,
		&g.DepositRate, &g.PayoutRate, &g.DepositExchangeRate, &g.PayoutExchangeRate,
		&g.ExchangeRateSource, &g.ExchangeRateRank, &g.ExchangeRateOffset, &g.FeeRate,
		&g.CutoffHour, &g.AllMembersCanRecord, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

func (s *Store) SetGroupActive(ctx context.Context, chatID int64, active bool, activeDayKey string, now time.Time) error {
	if !active {
		activeDayKey = ""
	}
	_, err := s.pool.Exec(ctx, `UPDATE groups SET active=$1, active_day_key=$2, updated_at=$3 WHERE chat_id=$4`,
		active, activeDayKey, now, chatID)
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

func (s *Store) SetGroupCutoffHour(ctx context.Context, chatID int64, cutoffHour int, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE groups SET cutoff_hour=$1, updated_at=$2 WHERE chat_id=$3`,
		cutoffHour, now, chatID)
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

func (s *Store) IsAnyOperator(ctx context.Context, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT 1 FROM operators WHERE user_id=$1 LIMIT 1`, userID)
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

func (s *Store) IsBroadcastOperator(ctx context.Context, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT 1 FROM broadcast_operators WHERE user_id=$1 AND status='active' LIMIT 1`, userID)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) UpsertBroadcastOperator(ctx context.Context, userID, createdBy int64, remark string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO broadcast_operators(user_id, status, created_by, remark, created_at, updated_at)
		VALUES($1, 'active', $2, $3, $4, $5)
		ON CONFLICT(user_id) DO UPDATE SET status='active', remark=excluded.remark, updated_at=excluded.updated_at`,
		userID, createdBy, remark, now, now)
	return err
}

func (s *Store) DisableBroadcastOperator(ctx context.Context, userID int64, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE broadcast_operators SET status='disabled', updated_at=$1 WHERE user_id=$2 AND status <> 'disabled'`,
		now, userID)
	return tag.RowsAffected() > 0, err
}

func (s *Store) ListBroadcastOperators(ctx context.Context) ([]BroadcastOperator, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id, status, remark, created_by, created_at, updated_at
		FROM broadcast_operators
		ORDER BY status ASC, updated_at DESC, user_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var operators []BroadcastOperator
	for rows.Next() {
		var op BroadcastOperator
		if err := rows.Scan(&op.UserID, &op.Status, &op.Remark, &op.CreatedBy, &op.CreatedAt, &op.UpdatedAt); err != nil {
			return nil, err
		}
		operators = append(operators, op)
	}
	return operators, rows.Err()
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
	rows, err := s.pool.Query(ctx, `SELECT g.chat_id, g.title, g.active, g.active_day_key, g.business_open, g.owner_user_id,
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
	if target == "chat" {
		groupName = ""
	} else {
		chatID = 0
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO broadcast_operator_permissions(user_id, target, chat_id, group_name, granted_by, created_at)
		VALUES($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`, userID, target, chatID, groupName, grantedBy, now)
	return err
}

func (s *Store) RemoveBroadcastPermission(ctx context.Context, userID int64, target string, chatID int64, groupName string) (bool, error) {
	target = strings.TrimSpace(target)
	groupName = strings.TrimSpace(groupName)
	if target == "chat" {
		groupName = ""
	} else {
		chatID = 0
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM broadcast_operator_permissions
		WHERE user_id=$1 AND target=$2 AND chat_id=$3 AND group_name=$4`, userID, target, chatID, groupName)
	return tag.RowsAffected() > 0, err
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
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT g.chat_id, g.title, g.active, g.active_day_key, g.business_open, g.owner_user_id,
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

func (s *Store) ListRecordsForDay(ctx context.Context, chatID int64, dayKey string) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
		subject_user_id, subject_name, actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
		FROM records
		WHERE chat_id=$1 AND day_key=$2 AND deleted_at IS NULL
		ORDER BY id ASC`, chatID, dayKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
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

func (s *Store) SoftDeleteRecordsForDay(ctx context.Context, chatID int64, dayKey string, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE records SET deleted_at=$1
		WHERE chat_id=$2 AND day_key=$3 AND deleted_at IS NULL`, now, chatID, dayKey)
	return tag.RowsAffected(), err
}

func (s *Store) SoftDeleteAllRecords(ctx context.Context, chatID int64, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE records SET deleted_at=$1
		WHERE chat_id=$2 AND deleted_at IS NULL`, now, chatID)
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
	tag, err := s.pool.Exec(ctx, `INSERT INTO chain_notifications(owner_user_id, address, tx_hash, direction, block_timestamp, created_at)
		VALUES($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`, owner, address, txHash, direction, blockTimestamp, now)
	return tag.RowsAffected() == 1, err
}

func (s *Store) RecordChainNotificationOutbox(ctx context.Context, owner int64, address, txHash, direction string, blockTimestamp int64, chatID int64, text, parseMode string, disablePreview bool, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	tag, err := tx.Exec(ctx, `INSERT INTO chain_notifications(owner_user_id, address, tx_hash, direction, block_timestamp, created_at)
		VALUES($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`, owner, address, txHash, direction, blockTimestamp, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	dedupeKey := fmt.Sprintf("chain:%d:%s:%s:%s", owner, address, txHash, direction)
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

func (s *Store) ClaimDueNotifications(ctx context.Context, limit int, now time.Time) ([]NotificationOutbox, error) {
	if limit < 1 {
		limit = 1
	}
	staleBefore := now.Add(-2 * time.Minute)
	rows, err := s.pool.Query(ctx, `WITH next AS (
			SELECT id
			FROM notification_outbox
			WHERE (status IN ('pending', 'failed') AND next_attempt_at <= $1)
				OR (status = 'sending' AND updated_at <= $3)
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
			n.created_at, n.updated_at, n.sent_at`, now, limit, staleBefore)
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
	if _, err := tx.Exec(ctx, `INSERT INTO chain_watcher_events(
			event_id, tx_hash, contract, from_address, to_address, value, token_symbol, token_address,
			token_decimals, block_timestamp, confirmed, source, created_at
		) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT(event_id) DO NOTHING`,
		event.EventID, event.TxHash, event.Contract, event.From, event.To, event.Value, event.TokenSymbol, event.TokenAddress,
		event.TokenDecimals, event.BlockTimestamp, event.Confirmed, event.Source, now); err != nil {
		return 0, err
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

func (s *Store) CleanupChainWatcherRetention(ctx context.Context, maxAge time.Duration, now time.Time) error {
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	cutoff := now.Add(-maxAge)
	if _, err := s.pool.Exec(ctx, `DELETE FROM chain_watcher_matched_events WHERE created_at < $1`, cutoff); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM chain_watcher_events WHERE created_at < $1`, cutoff)
	return err
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
