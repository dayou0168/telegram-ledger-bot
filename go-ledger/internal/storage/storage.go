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
			business_open BOOLEAN NOT NULL DEFAULT TRUE,
			owner_user_id BIGINT NOT NULL DEFAULT 0,
			deposit_rate TEXT NOT NULL DEFAULT '0',
			payout_rate TEXT NOT NULL DEFAULT '0',
			deposit_exchange_rate TEXT NOT NULL DEFAULT '1',
			payout_exchange_rate TEXT NOT NULL DEFAULT '1',
			fee_rate TEXT NOT NULL DEFAULT '0',
			cutoff_hour INTEGER NOT NULL DEFAULT 0,
			all_members_can_record BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_groups_updated_negative
			ON groups(updated_at DESC, chat_id)
			WHERE chat_id < 0`,
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
		`CREATE INDEX IF NOT EXISTS idx_records_chat_kind_active
			ON records(chat_id, kind, id DESC)
			WHERE deleted_at IS NULL`,
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
			active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY(owner_user_id, address)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_address_watches_active
			ON address_watches(active, owner_user_id, address)`,
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

func (s *Store) GetGroup(ctx context.Context, chatID int64) (Group, error) {
	row := s.pool.QueryRow(ctx, `SELECT chat_id, title, active, business_open, owner_user_id,
		deposit_rate, payout_rate, deposit_exchange_rate, payout_exchange_rate, fee_rate,
		cutoff_hour, all_members_can_record, created_at, updated_at
		FROM groups WHERE chat_id = $1`, chatID)
	return scanGroup(row)
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.pool.Query(ctx, `SELECT chat_id, title, active, business_open, owner_user_id,
		deposit_rate, payout_rate, deposit_exchange_rate, payout_exchange_rate, fee_rate,
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
	err := scanner.Scan(&g.ChatID, &g.Title, &g.Active, &g.BusinessOpen, &g.OwnerUserID,
		&g.DepositRate, &g.PayoutRate, &g.DepositExchangeRate, &g.PayoutExchangeRate, &g.FeeRate,
		&g.CutoffHour, &g.AllMembersCanRecord, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

func (s *Store) SetGroupActive(ctx context.Context, chatID int64, active bool, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE groups SET active=$1, updated_at=$2 WHERE chat_id=$3`,
		active, now, chatID)
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
	_, err := s.pool.Exec(ctx, `UPDATE groups SET deposit_exchange_rate=$1, payout_exchange_rate=$1, updated_at=$2 WHERE chat_id=$3`,
		rate, now, chatID)
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
	rows, err := s.pool.Query(ctx, `SELECT g.chat_id, g.title, g.active, g.business_open, g.owner_user_id,
		g.deposit_rate, g.payout_rate, g.deposit_exchange_rate, g.payout_exchange_rate, g.fee_rate,
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
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT g.chat_id, g.title, g.active, g.business_open, g.owner_user_id,
		g.deposit_rate, g.payout_rate, g.deposit_exchange_rate, g.payout_exchange_rate, g.fee_rate,
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
		chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt, actor_user_id, actor_name,
		source_message_id, bot_message_id, remark, created_at
	) VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	RETURNING id`,
		r.ChatID, r.DayKey, r.Kind, r.Currency, r.Amount, r.Rate, r.FeeRate, r.ResultUSDT, r.ActorUserID,
		r.ActorName, r.SourceMessageID, r.BotMessageID, r.Remark, r.CreatedAt).Scan(&id)
	return id, err
}

func (s *Store) SetRecordBotMessage(ctx context.Context, recordID, botMessageID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE records SET bot_message_id=$1 WHERE id=$2`, botMessageID, recordID)
	return err
}

func (s *Store) ListRecordsForDay(ctx context.Context, chatID int64, dayKey string) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
		actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
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
		actor_user_id, actor_name, source_message_id, bot_message_id, remark, created_at, deleted_at
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
		COALESCE(s.watch_income, TRUE), COALESCE(s.watch_expense, TRUE), COALESCE(s.notify_trx, TRUE),
		COALESCE(s.min_notify_amount, '0'), COALESCE(MAX(n.block_timestamp), 0)
		FROM address_watches w
		LEFT JOIN address_watch_settings s ON s.owner_user_id = w.owner_user_id
		LEFT JOIN chain_notifications n ON n.owner_user_id = w.owner_user_id AND n.address = w.address
		WHERE w.active = TRUE
		GROUP BY w.owner_user_id, w.address, w.label, s.watch_income, s.watch_expense, s.notify_trx, s.min_notify_amount
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
		COALESCE(s.watch_income, TRUE), COALESCE(s.watch_expense, TRUE), COALESCE(s.notify_trx, TRUE),
		COALESCE(s.min_notify_amount, '0'), COALESCE(MAX(n.block_timestamp), 0)
		FROM address_watches w
		LEFT JOIN address_watch_settings s ON s.owner_user_id = w.owner_user_id
		LEFT JOIN chain_notifications n ON n.owner_user_id = w.owner_user_id AND n.address = w.address
		WHERE w.active = TRUE AND w.owner_user_id=$1
		GROUP BY w.owner_user_id, w.address, w.label, s.watch_income, s.watch_expense, s.notify_trx, s.min_notify_amount
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
	_, err := s.pool.Exec(ctx, `INSERT INTO address_watches(owner_user_id, address, label, active, created_at, updated_at)
		VALUES($1, $2, $3, TRUE, $4, $5)
		ON CONFLICT(owner_user_id, address) DO UPDATE SET label=excluded.label, active=TRUE, updated_at=excluded.updated_at`,
		owner, address, label, now, now)
	return err
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
