package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const quickReplyOutboxErrorLimit = 1000

func insertQuickReplyOutboxTx(ctx context.Context, tx pgx.Tx, item TelegramInboxUpdate, input QuickReplyOutboxInsert, now time.Time) error {
	input.DedupeKey = strings.TrimSpace(input.DedupeKey)
	if input.DedupeKey == "" || input.ActorUserID <= 0 || input.SourceChatID == 0 || input.SourceMessageID <= 0 ||
		input.TargetChatID == 0 || input.TargetMessageID <= 0 {
		return errors.New("quick reply outbox item is incomplete")
	}
	var id int64
	err := tx.QueryRow(ctx, `INSERT INTO telegram_quick_reply_outbox(
		stream_key,inbox_update_id,dedupe_key,actor_user_id,source_chat_id,source_message_id,
		target_chat_id,target_message_id,state_version_update_id,status,attempts,next_attempt_at,
		lease_owner,last_error,created_at,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$2,'pending',0,$9,'','',$9,$9)
	ON CONFLICT DO NOTHING RETURNING id`, item.StreamKey, item.UpdateID, input.DedupeKey,
		input.ActorUserID, input.SourceChatID, input.SourceMessageID, input.TargetChatID, input.TargetMessageID, now).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var existing QuickReplyOutboxInsert
	var streamKey string
	var updateID, stateVersion int64
	err = tx.QueryRow(ctx, `SELECT stream_key,inbox_update_id,dedupe_key,actor_user_id,source_chat_id,
		source_message_id,target_chat_id,target_message_id,state_version_update_id
		FROM telegram_quick_reply_outbox
		WHERE dedupe_key=$1 OR (stream_key=$2 AND inbox_update_id=$3)
		ORDER BY CASE WHEN dedupe_key=$1 THEN 0 ELSE 1 END LIMIT 1`, input.DedupeKey, item.StreamKey, item.UpdateID).Scan(
		&streamKey, &updateID, &existing.DedupeKey, &existing.ActorUserID, &existing.SourceChatID,
		&existing.SourceMessageID, &existing.TargetChatID, &existing.TargetMessageID, &stateVersion)
	if err != nil {
		return err
	}
	if streamKey != item.StreamKey || updateID != item.UpdateID || stateVersion != item.UpdateID || existing != input {
		return errors.New("quick reply outbox dedupe key belongs to a different delivery")
	}
	return nil
}

func (s *Store) ClaimQuickReplyOutbox(ctx context.Context, owner string, limit, maxAttempts int, lease time.Duration, now time.Time) ([]QuickReplyOutbox, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, errors.New("quick reply outbox owner is required")
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
	if _, err := s.pool.Exec(ctx, `UPDATE telegram_quick_reply_outbox
		SET status='dead',lease_owner='',lease_until=NULL,completed_at=$1,updated_at=$1,
			last_error=CASE WHEN last_error='' THEN 'maximum attempts exhausted after lease expiry' ELSE last_error END
		WHERE status='processing' AND lease_until <= $1 AND attempts >= $2`, now, maxAttempts); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `WITH candidates AS (
		SELECT q.id,q.status
		FROM telegram_quick_reply_outbox q
		WHERE q.attempts < $3
		  AND ((q.status IN ('pending','retry') AND q.next_attempt_at <= $1)
		       OR (q.status='processing' AND q.lease_until <= $1))
		  AND NOT EXISTS (
			SELECT 1 FROM telegram_quick_reply_outbox earlier
			WHERE earlier.stream_key=q.stream_key AND earlier.actor_user_id=q.actor_user_id AND earlier.id<q.id
			  AND earlier.status IN ('pending','processing','retry')
		  )
		ORDER BY q.id
		FOR UPDATE SKIP LOCKED
		LIMIT $2
	) UPDATE telegram_quick_reply_outbox q
	SET status='processing',attempts=q.attempts+1,lease_owner=$4,lease_until=$1+$5::interval,
		updated_at=$1,last_error=CASE WHEN candidates.status='processing' THEN q.last_error ELSE '' END
	FROM candidates WHERE q.id=candidates.id
	RETURNING q.id,q.stream_key,q.inbox_update_id,q.dedupe_key,q.actor_user_id,q.source_chat_id,
		q.source_message_id,q.target_chat_id,q.target_message_id,q.state_version_update_id,q.status,
		q.attempts,q.next_attempt_at,q.lease_owner,q.lease_until,q.last_error,q.created_at,q.updated_at,q.completed_at`,
		now, limit, maxAttempts, owner, durationInterval(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []QuickReplyOutbox
	for rows.Next() {
		item, err := scanQuickReplyOutbox(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) RenewQuickReplyOutboxLease(ctx context.Context, id int64, owner string, lease time.Duration, now time.Time) (bool, error) {
	if lease <= 0 {
		lease = time.Minute
	}
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_quick_reply_outbox
		SET lease_until=$3+$4::interval,updated_at=$3
		WHERE id=$1 AND status='processing' AND lease_owner=$2 AND lease_until > $3`,
		id, strings.TrimSpace(owner), now, durationInterval(lease))
	return tag.RowsAffected() == 1, err
}

func (s *Store) MarkQuickReplyOutboxSent(ctx context.Context, id int64, owner string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_quick_reply_outbox
		SET status='sent',lease_owner='',lease_until=NULL,last_error='',completed_at=$3,updated_at=$3
		WHERE id=$1 AND status='processing' AND lease_owner=$2 AND lease_until > $3`, id, strings.TrimSpace(owner), now)
	return tag.RowsAffected() == 1, err
}

func (s *Store) RetryQuickReplyOutbox(ctx context.Context, id int64, owner string, maxAttempts int, nextAttemptAt, now time.Time, cause error) (string, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	message := ""
	if cause != nil {
		message = strings.TrimSpace(cause.Error())
		if len(message) > quickReplyOutboxErrorLimit {
			message = message[:quickReplyOutboxErrorLimit]
		}
	}
	var status string
	err := s.pool.QueryRow(ctx, `UPDATE telegram_quick_reply_outbox
		SET status=CASE WHEN attempts >= $3 THEN 'dead' ELSE 'retry' END,
			next_attempt_at=CASE WHEN attempts >= $3 THEN next_attempt_at ELSE $4 END,
			lease_owner='',lease_until=NULL,last_error=$5,updated_at=$6,
			completed_at=CASE WHEN attempts >= $3 THEN $6 ELSE NULL END
		WHERE id=$1 AND status='processing' AND lease_owner=$2 AND lease_until > $6
		RETURNING status`, id, strings.TrimSpace(owner), maxAttempts, nextAttemptAt, message, now).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, err
}

func (s *Store) MarkQuickReplyOutboxDead(ctx context.Context, id int64, owner string, now time.Time, cause error) (bool, error) {
	message := ""
	if cause != nil {
		message = strings.TrimSpace(cause.Error())
		if len(message) > quickReplyOutboxErrorLimit {
			message = message[:quickReplyOutboxErrorLimit]
		}
	}
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_quick_reply_outbox
		SET status='dead',lease_owner='',lease_until=NULL,last_error=$3,completed_at=$4,updated_at=$4
		WHERE id=$1 AND status='processing' AND lease_owner=$2 AND lease_until > $4`,
		id, strings.TrimSpace(owner), message, now)
	return tag.RowsAffected() == 1, err
}

func (s *Store) CancelQuickReplyOutboxRevoked(ctx context.Context, id int64, owner, reason string, now time.Time) (bool, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer rollback(ctx, tx)
	var streamKey string
	var actorUserID, stateVersion int64
	err = tx.QueryRow(ctx, `SELECT stream_key,actor_user_id,state_version_update_id
		FROM telegram_quick_reply_outbox
		WHERE id=$1 AND status='processing' AND lease_owner=$2 AND lease_until > $3
		FOR UPDATE`, id, strings.TrimSpace(owner), now).Scan(&streamKey, &actorUserID, &stateVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, tx.Commit(ctx)
	}
	if err != nil {
		return false, false, err
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > quickReplyOutboxErrorLimit {
		reason = reason[:quickReplyOutboxErrorLimit]
	}
	stateTag, err := tx.Exec(ctx, `UPDATE telegram_private_route_states
		SET state_json='{}'::jsonb,has_state=FALSE,updated_at=$4
		WHERE stream_key=$1 AND user_id=$2 AND version_update_id=$3`, streamKey, actorUserID, stateVersion, now)
	if err != nil {
		return false, false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE telegram_quick_reply_outbox
		SET status='cancelled',lease_owner='',lease_until=NULL,last_error=$3,completed_at=$4,updated_at=$4
		WHERE id=$1 AND status='processing' AND lease_owner=$2 AND lease_until > $4`,
		id, strings.TrimSpace(owner), reason, now)
	if err != nil {
		return false, false, err
	}
	if tag.RowsAffected() != 1 {
		return false, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	return true, stateTag.RowsAffected() == 1, nil
}

func (s *Store) CleanupQuickReplyOutbox(ctx context.Context, completedBefore time.Time, limit int) (int64, error) {
	if limit < 1 {
		limit = 1000
	}
	tag, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT id FROM telegram_quick_reply_outbox
		WHERE status IN ('sent','cancelled','dead') AND completed_at < $1
		ORDER BY completed_at,id LIMIT $2
	) DELETE FROM telegram_quick_reply_outbox q USING expired e WHERE q.id=e.id`, completedBefore, limit)
	return tag.RowsAffected(), err
}

func (s *Store) GetQuickReplyOutboxByDedupe(ctx context.Context, dedupeKey string) (QuickReplyOutbox, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT id,stream_key,inbox_update_id,dedupe_key,actor_user_id,source_chat_id,
		source_message_id,target_chat_id,target_message_id,state_version_update_id,status,attempts,
		next_attempt_at,lease_owner,lease_until,last_error,created_at,updated_at,completed_at
		FROM telegram_quick_reply_outbox WHERE dedupe_key=$1`, strings.TrimSpace(dedupeKey))
	item, err := scanQuickReplyOutbox(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return QuickReplyOutbox{}, false, nil
	}
	return item, err == nil, err
}

type quickReplyOutboxScanner interface {
	Scan(dest ...any) error
}

func scanQuickReplyOutbox(scanner quickReplyOutboxScanner) (QuickReplyOutbox, error) {
	var item QuickReplyOutbox
	var leaseUntil, completedAt pgtype.Timestamptz
	err := scanner.Scan(&item.ID, &item.StreamKey, &item.InboxUpdateID, &item.DedupeKey, &item.ActorUserID,
		&item.SourceChatID, &item.SourceMessageID, &item.TargetChatID, &item.TargetMessageID,
		&item.StateVersionUpdateID, &item.Status, &item.Attempts, &item.NextAttemptAt, &item.LeaseOwner,
		&leaseUntil, &item.LastError, &item.CreatedAt, &item.UpdatedAt, &completedAt)
	if err != nil {
		return QuickReplyOutbox{}, err
	}
	if leaseUntil.Valid {
		value := leaseUntil.Time
		item.LeaseUntil = &value
	}
	if completedAt.Valid {
		value := completedAt.Time
		item.CompletedAt = &value
	}
	return item, nil
}
