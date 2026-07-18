package storage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClaimNotificationByID competes with the generic SKIP LOCKED scheduler on
// the same row. Earlier critical rows for the same chat retain send order.
func (s *Store) ClaimNotificationByID(ctx context.Context, id int64, maxAttempts int, now time.Time) (NotificationOutbox, bool, error) {
	if id <= 0 {
		return NotificationOutbox{}, false, nil
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	staleBefore := now.Add(-2 * time.Minute)
	row := s.pool.QueryRow(ctx, `WITH next AS (
		SELECT n.id FROM notification_outbox n
		WHERE n.id=$1
		  AND ((n.status IN ('pending','failed') AND n.next_attempt_at <= $2)
		    OR (n.status='sending' AND n.updated_at <= $3))
		  AND n.attempts < $4
		  AND NOT EXISTS (
			SELECT 1 FROM notification_outbox earlier
			WHERE earlier.chat_id=n.chat_id AND earlier.priority=0 AND earlier.id<n.id
			  AND earlier.status<>'sent' AND earlier.attempts<$4
		  )
		FOR UPDATE SKIP LOCKED
	) UPDATE notification_outbox n SET status='sending',attempts=n.attempts+1,updated_at=$2
	FROM next WHERE n.id=next.id
	RETURNING n.id,n.kind,n.dedupe_key,n.chat_id,n.text,n.payload_type,n.file_id,n.caption,n.parse_mode,n.disable_preview,
		n.reply_to_message_id,n.reply_to_upstream_id,n.reply_markup_json,n.reference_kind,n.reference_id,n.priority,
		n.status,n.attempts,n.next_attempt_at,n.last_error,n.created_at,n.updated_at,n.sent_at`,
		id, now, staleBefore, maxAttempts)
	var item NotificationOutbox
	err := row.Scan(&item.ID, &item.Kind, &item.DedupeKey, &item.ChatID, &item.Text,
		&item.PayloadType, &item.FileID, &item.Caption, &item.ParseMode, &item.DisablePreview, &item.ReplyToMessageID, &item.ReplyToUpstreamID,
		&item.ReplyMarkupJSON, &item.ReferenceKind, &item.ReferenceID, &item.Priority,
		&item.Status, &item.Attempts, &item.NextAttemptAt, &item.LastError,
		&item.CreatedAt, &item.UpdatedAt, &item.SentAt)
	if err == pgx.ErrNoRows {
		return NotificationOutbox{}, false, nil
	}
	return item, err == nil, err
}
