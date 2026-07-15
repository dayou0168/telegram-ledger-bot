package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const telegramInboxErrorLimit = 2000

func (s *Store) GetTelegramPollOffset(ctx context.Context, streamKey string) (int64, error) {
	streamKey = strings.TrimSpace(streamKey)
	if streamKey == "" {
		return 0, errors.New("telegram stream key is required")
	}
	var offset int64
	err := s.pool.QueryRow(ctx, `SELECT next_offset FROM telegram_poll_state WHERE stream_key=$1`, streamKey).Scan(&offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return offset, err
}

// PersistTelegramUpdateBatch stores every update and advances the Telegram poll
// offset in one transaction. Existing processed_updates rows are imported as
// done so upgrading does not replay updates completed by the legacy consumer.
func (s *Store) PersistTelegramUpdateBatch(ctx context.Context, streamKey string, updates []TelegramInboxUpdate, now time.Time) (int64, error) {
	streamKey = strings.TrimSpace(streamKey)
	if streamKey == "" {
		return 0, errors.New("telegram stream key is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(ctx, tx)
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_poll_state(stream_key,next_offset,updated_at)
		VALUES($1,0,$2) ON CONFLICT(stream_key) DO NOTHING`, streamKey, now); err != nil {
		return 0, err
	}
	var nextOffset int64
	if err := tx.QueryRow(ctx, `SELECT next_offset FROM telegram_poll_state WHERE stream_key=$1 FOR UPDATE`, streamKey).Scan(&nextOffset); err != nil {
		return 0, err
	}
	type batchRow struct {
		UpdateID int64           `json:"update_id"`
		Payload  json.RawMessage `json:"payload"`
		Lane     string          `json:"lane"`
		RouteKey string          `json:"route_key"`
	}
	batch := make([]batchRow, 0, len(updates))
	for _, update := range updates {
		lane := strings.TrimSpace(update.Lane)
		if update.UpdateID < 0 || len(update.Payload) == 0 || (lane != "ledger" && lane != "bypass") || strings.TrimSpace(update.RouteKey) == "" {
			return 0, fmt.Errorf("telegram inbox update %d is incomplete", update.UpdateID)
		}
		batch = append(batch, batchRow{UpdateID: update.UpdateID, Payload: json.RawMessage(update.Payload), Lane: lane, RouteKey: strings.TrimSpace(update.RouteKey)})
		if candidate := update.UpdateID + 1; candidate > nextOffset {
			nextOffset = candidate
		}
	}
	if len(batch) > 0 {
		payload, err := json.Marshal(batch)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `WITH incoming AS (
			SELECT * FROM jsonb_to_recordset($2::jsonb) AS x(
				update_id BIGINT,payload JSONB,lane TEXT,route_key TEXT
			)
		) INSERT INTO telegram_update_inbox(
			stream_key,update_id,payload,lane,route_key,status,attempts,next_attempt_at,
			lease_owner,last_error,created_at,updated_at,done_at
		) SELECT $1,i.update_id,i.payload,i.lane,i.route_key,
			CASE WHEN p.update_id IS NULL THEN 'pending' ELSE 'done' END,
			0,$3::timestamptz,'','',$3::timestamptz,$3::timestamptz,
			CASE WHEN p.update_id IS NULL THEN NULL ELSE $3::timestamptz END
		FROM incoming i LEFT JOIN processed_updates p ON p.update_id=i.update_id
		ON CONFLICT(stream_key,update_id) DO NOTHING`, streamKey, payload, now); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_poll_state SET next_offset=$2,updated_at=$3 WHERE stream_key=$1`, streamKey, nextOffset, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return nextOffset, nil
}

func (s *Store) ClaimTelegramUpdates(ctx context.Context, streamKey, lane, owner string, limit, maxAttempts int, lease time.Duration, now time.Time) ([]TelegramInboxUpdate, error) {
	streamKey = strings.TrimSpace(streamKey)
	lane = strings.TrimSpace(lane)
	owner = strings.TrimSpace(owner)
	if streamKey == "" || owner == "" || (lane != "ledger" && lane != "bypass") {
		return nil, errors.New("telegram inbox claim identity is incomplete")
	}
	if limit < 1 {
		limit = 1
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if lease <= 0 {
		lease = time.Minute
	}
	if _, err := s.pool.Exec(ctx, `UPDATE telegram_update_inbox
		SET status='dead',lease_owner='',lease_until=NULL,updated_at=$4,
			last_error=CASE WHEN last_error='' THEN 'maximum attempts exhausted after lease expiry' ELSE last_error END
		WHERE stream_key=$1 AND lane=$2 AND status='processing' AND lease_until <= $4 AND attempts >= $3`,
		streamKey, lane, maxAttempts, now); err != nil {
		return nil, err
	}
	ordered := lane == "ledger"
	rows, err := s.pool.Query(ctx, `WITH candidates AS (
		SELECT i.stream_key,i.update_id,i.status
		FROM telegram_update_inbox i
		WHERE i.stream_key=$1 AND i.lane=$2 AND i.attempts < $5
		  AND ((i.status IN ('pending','retry') AND i.next_attempt_at <= $4)
		       OR (i.status='processing' AND i.lease_until <= $4))
		  AND (NOT $7 OR NOT EXISTS (
			SELECT 1 FROM telegram_update_inbox earlier
			WHERE earlier.stream_key=i.stream_key AND earlier.lane=i.lane
			  AND earlier.route_key=i.route_key AND earlier.update_id < i.update_id
			  AND earlier.status NOT IN ('done','dead')
		  ))
		ORDER BY i.update_id
		FOR UPDATE SKIP LOCKED
		LIMIT $3
	) UPDATE telegram_update_inbox i
	SET status='processing',attempts=i.attempts+1,lease_owner=$6,lease_until=$4+$8::interval,
		lease_reclaims=i.lease_reclaims+CASE WHEN candidates.status='processing' THEN 1 ELSE 0 END,
		updated_at=$4
	FROM candidates
	WHERE i.stream_key=candidates.stream_key AND i.update_id=candidates.update_id
	RETURNING i.stream_key,i.update_id,i.payload,i.lane,i.route_key,i.status,i.attempts,
		i.next_attempt_at,i.lease_owner,i.lease_until,i.last_error,i.lease_reclaims,
		i.handled_at,i.created_at,i.updated_at,i.done_at`, streamKey, lane, limit, now,
		maxAttempts, owner, ordered, durationInterval(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claimed []TelegramInboxUpdate
	for rows.Next() {
		item, err := scanTelegramInboxUpdate(rows)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, item)
	}
	return claimed, rows.Err()
}

func (s *Store) MarkTelegramUpdateHandled(ctx context.Context, streamKey string, updateID int64, owner string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_update_inbox SET handled_at=$4,updated_at=$4
		WHERE stream_key=$1 AND update_id=$2 AND status='processing' AND lease_owner=$3
		  AND lease_until > $4`,
		strings.TrimSpace(streamKey), updateID, strings.TrimSpace(owner), now)
	return tag.RowsAffected() == 1, err
}

func (s *Store) RenewTelegramUpdateLease(ctx context.Context, streamKey string, updateID int64, owner string, lease time.Duration, now time.Time) (bool, error) {
	if lease <= 0 {
		lease = time.Minute
	}
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_update_inbox SET lease_until=$4+$5::interval,updated_at=$4
		WHERE stream_key=$1 AND update_id=$2 AND status='processing' AND lease_owner=$3
		  AND lease_until > $4`,
		strings.TrimSpace(streamKey), updateID, strings.TrimSpace(owner), now, durationInterval(lease))
	return tag.RowsAffected() == 1, err
}

func (s *Store) CompleteTelegramUpdate(ctx context.Context, streamKey string, updateID int64, owner string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_update_inbox
		SET status='done',lease_owner='',lease_until=NULL,last_error='',done_at=$4,updated_at=$4
		WHERE stream_key=$1 AND update_id=$2 AND status='processing' AND lease_owner=$3
		  AND handled_at IS NOT NULL AND lease_until > $4`,
		strings.TrimSpace(streamKey), updateID, strings.TrimSpace(owner), now)
	return tag.RowsAffected() == 1, err
}

func (s *Store) RetryTelegramUpdate(ctx context.Context, streamKey string, updateID int64, owner string, maxAttempts int, retryAt, now time.Time, cause error) (string, error) {
	lastError := ""
	if cause != nil {
		lastError = truncateTelegramInboxError(cause.Error())
	}
	var status string
	err := s.pool.QueryRow(ctx, `UPDATE telegram_update_inbox
		SET status=CASE WHEN attempts >= $4 THEN 'dead' ELSE 'retry' END,
			next_attempt_at=CASE WHEN attempts >= $4 THEN next_attempt_at ELSE $5 END,
			lease_owner='',lease_until=NULL,last_error=$6,updated_at=$7
		WHERE stream_key=$1 AND update_id=$2 AND status='processing' AND lease_owner=$3
		  AND lease_until > $7
		RETURNING status`, strings.TrimSpace(streamKey), updateID, strings.TrimSpace(owner), maxAttempts,
		retryAt, lastError, now).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, err
}

func (s *Store) GetTelegramPrivateRouteState(ctx context.Context, streamKey string, userID int64) (TelegramPrivateRouteState, bool, error) {
	var state TelegramPrivateRouteState
	err := s.pool.QueryRow(ctx, `SELECT stream_key,user_id,state_json,has_state,version_update_id,updated_at
		FROM telegram_private_route_states WHERE stream_key=$1 AND user_id=$2`,
		strings.TrimSpace(streamKey), userID).Scan(&state.StreamKey, &state.UserID, &state.StateJSON,
		&state.HasState, &state.VersionUpdateID, &state.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TelegramPrivateRouteState{StreamKey: strings.TrimSpace(streamKey), UserID: userID, VersionUpdateID: -1}, false, nil
	}
	return state, err == nil, err
}

func (s *Store) CommitTelegramPrivateStateAndMarkHandled(ctx context.Context, item TelegramInboxUpdate, owner string, userID, expectedVersion int64, stateJSON []byte, hasState bool, now time.Time) (bool, error) {
	return s.CommitTelegramPrivateStateHandledAndQuickReply(ctx, item, owner, userID, expectedVersion, stateJSON, hasState, nil, now)
}

func (s *Store) CommitTelegramPrivateStateHandledAndQuickReply(ctx context.Context, item TelegramInboxUpdate, owner string, userID, expectedVersion int64, stateJSON []byte, hasState bool, quickReply *QuickReplyOutboxInsert, now time.Time) (bool, error) {
	if strings.TrimSpace(item.StreamKey) == "" || item.UpdateID < 0 || strings.TrimSpace(owner) == "" || userID <= 0 || item.UpdateID <= expectedVersion {
		return false, errors.New("telegram private state commit is incomplete")
	}
	if len(stateJSON) == 0 {
		stateJSON = []byte(`{}`)
	}
	if !json.Valid(stateJSON) {
		return false, errors.New("telegram private state is not valid JSON")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	var owned int
	err = tx.QueryRow(ctx, `SELECT 1 FROM telegram_update_inbox
		WHERE stream_key=$1 AND update_id=$2 AND status='processing' AND lease_owner=$3
		  AND lease_until > $4 AND handled_at IS NULL
		FOR UPDATE`, item.StreamKey, item.UpdateID, strings.TrimSpace(owner), now).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	var currentVersion int64
	err = tx.QueryRow(ctx, `SELECT version_update_id FROM telegram_private_route_states
		WHERE stream_key=$1 AND user_id=$2 FOR UPDATE`, item.StreamKey, userID).Scan(&currentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		currentVersion = -1
	} else if err != nil {
		return false, err
	}
	if currentVersion != expectedVersion || item.UpdateID <= currentVersion {
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_private_route_states(
		stream_key,user_id,state_json,has_state,version_update_id,updated_at
	) VALUES($1,$2,$3::jsonb,$4,$5,$6)
	ON CONFLICT(stream_key,user_id) DO UPDATE SET
		state_json=excluded.state_json,has_state=excluded.has_state,
		version_update_id=excluded.version_update_id,updated_at=excluded.updated_at`,
		item.StreamKey, userID, stateJSON, hasState, item.UpdateID, now); err != nil {
		return false, err
	}
	if quickReply != nil {
		if err := insertQuickReplyOutboxTx(ctx, tx, item, *quickReply, now); err != nil {
			return false, err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE telegram_update_inbox SET handled_at=$4,updated_at=$4
		WHERE stream_key=$1 AND update_id=$2 AND status='processing' AND lease_owner=$3
		  AND lease_until > $4 AND handled_at IS NULL`, item.StreamKey, item.UpdateID, strings.TrimSpace(owner), now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, tx.Commit(ctx)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) DeleteTelegramPrivateRouteState(ctx context.Context, streamKey string, userID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM telegram_private_route_states WHERE stream_key=$1 AND user_id=$2`, strings.TrimSpace(streamKey), userID)
	return err
}

func (s *Store) DeleteAllTelegramPrivateRouteStates(ctx context.Context, streamKey string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM telegram_private_route_states WHERE stream_key=$1`, strings.TrimSpace(streamKey))
	return err
}

func (s *Store) ClearTelegramPrivateRouteStateVersion(ctx context.Context, streamKey string, userID, versionUpdateID int64, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_private_route_states
		SET state_json='{}'::jsonb,has_state=FALSE,updated_at=$4
		WHERE stream_key=$1 AND user_id=$2 AND version_update_id=$3`,
		strings.TrimSpace(streamKey), userID, versionUpdateID, now)
	return tag.RowsAffected() == 1, err
}

func (s *Store) TelegramInboxStats(ctx context.Context, streamKey string, now time.Time) (TelegramInboxStats, error) {
	var stats TelegramInboxStats
	var oldest pgtype.Timestamptz
	err := s.pool.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE status='pending'),
		COUNT(*) FILTER (WHERE status='processing'),
		COUNT(*) FILTER (WHERE status='retry'),
		COUNT(*) FILTER (WHERE status='dead'),
		MIN(created_at) FILTER (WHERE status NOT IN ('done','dead')),
		COALESCE(SUM(lease_reclaims),0)
		FROM telegram_update_inbox WHERE stream_key=$1`, strings.TrimSpace(streamKey)).Scan(
		&stats.Pending, &stats.Processing, &stats.Retry, &stats.Dead, &oldest, &stats.LeaseReclaims)
	if err == nil && oldest.Valid && now.After(oldest.Time) {
		stats.OldestAge = now.Sub(oldest.Time)
	}
	return stats, err
}

func (s *Store) CleanupDoneTelegramUpdates(ctx context.Context, streamKey string, cutoff time.Time, limit int) (int64, error) {
	if limit < 1 {
		limit = 1000
	}
	tag, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT stream_key,update_id FROM telegram_update_inbox
		WHERE stream_key=$1 AND status='done' AND done_at < $2
		ORDER BY done_at,update_id LIMIT $3
	) DELETE FROM telegram_update_inbox i USING expired e
	WHERE i.stream_key=e.stream_key AND i.update_id=e.update_id`, strings.TrimSpace(streamKey), cutoff, limit)
	return tag.RowsAffected(), err
}

func scanTelegramInboxUpdate(scanner recordScanner) (TelegramInboxUpdate, error) {
	var item TelegramInboxUpdate
	var leaseUntil, handledAt, doneAt pgtype.Timestamptz
	err := scanner.Scan(&item.StreamKey, &item.UpdateID, &item.Payload, &item.Lane, &item.RouteKey,
		&item.Status, &item.Attempts, &item.NextAttemptAt, &item.LeaseOwner, &leaseUntil,
		&item.LastError, &item.LeaseReclaims, &handledAt, &item.CreatedAt, &item.UpdatedAt, &doneAt)
	if err != nil {
		return TelegramInboxUpdate{}, err
	}
	if leaseUntil.Valid {
		value := leaseUntil.Time
		item.LeaseUntil = &value
	}
	if handledAt.Valid {
		value := handledAt.Time
		item.HandledAt = &value
	}
	if doneAt.Valid {
		value := doneAt.Time
		item.DoneAt = &value
	}
	return item, nil
}

func durationInterval(value time.Duration) string {
	return fmt.Sprintf("%f seconds", value.Seconds())
}

func truncateTelegramInboxError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > telegramInboxErrorLimit {
		return value[:telegramInboxErrorLimit]
	}
	return value
}
