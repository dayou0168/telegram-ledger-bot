package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) UpsertTelegramBroadcastTarget(ctx context.Context, target TelegramBroadcastTarget, now time.Time) error {
	target.StreamKey = strings.TrimSpace(target.StreamKey)
	target.Mode = strings.TrimSpace(target.Mode)
	target.TargetName = strings.TrimSpace(target.TargetName)
	if target.StreamKey == "" || target.UserID <= 0 {
		return errors.New("telegram broadcast target identity is incomplete")
	}
	switch target.Mode {
	case "all":
		target.ChatID = 0
		target.GroupID = 0
	case "group":
		if target.GroupID <= 0 {
			return errors.New("telegram broadcast group target is incomplete")
		}
		target.ChatID = 0
	case "chat":
		if target.ChatID == 0 {
			return errors.New("telegram broadcast chat target is incomplete")
		}
		target.GroupID = 0
	default:
		return errors.New("telegram broadcast target mode is invalid")
	}
	var groupID any
	if target.GroupID > 0 {
		groupID = target.GroupID
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO telegram_broadcast_targets(
		stream_key,user_id,mode,chat_id,group_id,target_name,notify_all,updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
	ON CONFLICT(stream_key,user_id) DO UPDATE SET
		mode=excluded.mode,chat_id=excluded.chat_id,group_id=excluded.group_id,
		target_name=excluded.target_name,notify_all=excluded.notify_all,updated_at=excluded.updated_at`,
		target.StreamKey, target.UserID, target.Mode, target.ChatID, groupID,
		target.TargetName, target.NotifyAll, now)
	return err
}

func (s *Store) GetTelegramBroadcastTarget(ctx context.Context, streamKey string, userID int64) (TelegramBroadcastTarget, bool, error) {
	var target TelegramBroadcastTarget
	var groupID *int64
	err := s.pool.QueryRow(ctx, `SELECT target.stream_key,target.user_id,target.mode,target.chat_id,target.group_id,
		CASE WHEN target.mode='group' THEN COALESCE(bg.name,'') ELSE target.target_name END,
		target.notify_all,target.updated_at
		FROM telegram_broadcast_targets target
		LEFT JOIN broadcast_groups bg ON bg.id=target.group_id
		WHERE target.stream_key=$1 AND target.user_id=$2`, strings.TrimSpace(streamKey), userID).Scan(
		&target.StreamKey, &target.UserID, &target.Mode, &target.ChatID, &groupID,
		&target.TargetName, &target.NotifyAll, &target.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TelegramBroadcastTarget{}, false, nil
	}
	if err != nil {
		return TelegramBroadcastTarget{}, false, err
	}
	if groupID != nil {
		target.GroupID = *groupID
	}
	return target, true, nil
}

func (s *Store) DeleteTelegramBroadcastTarget(ctx context.Context, streamKey string, userID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM telegram_broadcast_targets WHERE stream_key=$1 AND user_id=$2`, strings.TrimSpace(streamKey), userID)
	return err
}

func (s *Store) EnqueueBroadcastUpstreamMessage(ctx context.Context, item NotificationOutbox, sourceOperatorUserID, sourceChatID, sourceMessageID, recipientUserID int64, now time.Time, companionItems ...NotificationOutbox) (bool, BroadcastUpstreamMessage, error) {
	item.Kind = strings.TrimSpace(item.Kind)
	item.DedupeKey = strings.TrimSpace(item.DedupeKey)
	item.PayloadType = strings.TrimSpace(item.PayloadType)
	if item.PayloadType == "" {
		item.PayloadType = "text"
	}
	if item.Kind == "" || item.DedupeKey == "" || sourceOperatorUserID <= 0 || sourceChatID <= 0 || sourceMessageID <= 0 || recipientUserID <= 0 {
		return false, BroadcastUpstreamMessage{}, errors.New("broadcast upstream message is incomplete")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, BroadcastUpstreamMessage{}, err
	}
	defer rollback(ctx, tx)
	insertOutbox := func(candidate NotificationOutbox) (bool, int64, error) {
		candidate.Kind = strings.TrimSpace(candidate.Kind)
		candidate.DedupeKey = strings.TrimSpace(candidate.DedupeKey)
		candidate.PayloadType = strings.TrimSpace(candidate.PayloadType)
		if candidate.PayloadType == "" {
			candidate.PayloadType = "text"
		}
		if candidate.Kind == "" || candidate.DedupeKey == "" {
			return false, 0, errors.New("broadcast upstream companion is incomplete")
		}
		var outboxID int64
		inserted := true
		err := tx.QueryRow(ctx, `INSERT INTO notification_outbox(
			kind,dedupe_key,chat_id,text,payload_type,file_id,caption,parse_mode,disable_preview,
			reply_to_message_id,reply_to_upstream_id,reply_markup_json,reference_kind,reference_id,
			priority,status,attempts,next_attempt_at,created_at,updated_at
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'pending',0,$16,$16,$16)
		ON CONFLICT(dedupe_key) DO NOTHING RETURNING id`, candidate.Kind, candidate.DedupeKey, candidate.ChatID,
			candidate.Text, candidate.PayloadType, candidate.FileID, candidate.Caption, candidate.ParseMode,
			candidate.DisablePreview, candidate.ReplyToMessageID, candidate.ReplyToUpstreamID,
			candidate.ReplyMarkupJSON, candidate.ReferenceKind, candidate.ReferenceID, candidate.Priority, now).Scan(&outboxID)
		if errors.Is(err, pgx.ErrNoRows) {
			inserted = false
			err = tx.QueryRow(ctx, `SELECT id FROM notification_outbox WHERE dedupe_key=$1`, candidate.DedupeKey).Scan(&outboxID)
		}
		return inserted, outboxID, err
	}
	var existing BroadcastUpstreamMessage
	err = tx.QueryRow(ctx, `SELECT id,source_operator_user_id,source_chat_id,source_message_id,recipient_user_id,
		outbox_id,telegram_message_id,created_at,sent_at FROM broadcast_upstream_messages
		WHERE source_chat_id=$1 AND source_message_id=$2 AND recipient_user_id=$3 FOR UPDATE`,
		sourceChatID, sourceMessageID, recipientUserID).Scan(&existing.ID, &existing.SourceOperatorUserID,
		&existing.SourceChatID, &existing.SourceMessageID, &existing.RecipientUserID, &existing.OutboxID,
		&existing.TelegramMessageID, &existing.CreatedAt, &existing.SentAt)
	if err == nil {
		inserted := false
		for _, companion := range companionItems {
			companion.ChatID = recipientUserID
			companion.ReplyToUpstreamID = existing.ID
			companionInserted, _, companionErr := insertOutbox(companion)
			if companionErr != nil {
				return false, BroadcastUpstreamMessage{}, companionErr
			}
			inserted = inserted || companionInserted
		}
		return inserted, existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, BroadcastUpstreamMessage{}, err
	}
	inserted, outboxID, err := insertOutbox(item)
	if err != nil {
		return false, BroadcastUpstreamMessage{}, err
	}
	var upstream BroadcastUpstreamMessage
	err = tx.QueryRow(ctx, `INSERT INTO broadcast_upstream_messages(
		source_operator_user_id,source_chat_id,source_message_id,recipient_user_id,outbox_id,created_at
	) VALUES($1,$2,$3,$4,$5,$6)
	ON CONFLICT(source_chat_id,source_message_id,recipient_user_id) DO UPDATE SET
		outbox_id=broadcast_upstream_messages.outbox_id
	RETURNING id,source_operator_user_id,source_chat_id,source_message_id,recipient_user_id,
		outbox_id,telegram_message_id,created_at,sent_at`, sourceOperatorUserID, sourceChatID,
		sourceMessageID, recipientUserID, outboxID, now).Scan(&upstream.ID, &upstream.SourceOperatorUserID,
		&upstream.SourceChatID, &upstream.SourceMessageID, &upstream.RecipientUserID, &upstream.OutboxID,
		&upstream.TelegramMessageID, &upstream.CreatedAt, &upstream.SentAt)
	if err != nil {
		return false, BroadcastUpstreamMessage{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE notification_outbox SET reference_kind='broadcast_upstream',reference_id=$2 WHERE id=$1`, outboxID, upstream.ID); err != nil {
		return false, BroadcastUpstreamMessage{}, err
	}
	for _, companion := range companionItems {
		companion.ChatID = recipientUserID
		companion.ReplyToUpstreamID = upstream.ID
		companionInserted, _, companionErr := insertOutbox(companion)
		if companionErr != nil {
			return false, BroadcastUpstreamMessage{}, companionErr
		}
		inserted = inserted || companionInserted
	}
	if err := tx.Commit(ctx); err != nil {
		return false, BroadcastUpstreamMessage{}, err
	}
	return inserted, upstream, nil
}

func (s *Store) GetBroadcastUpstreamMessage(ctx context.Context, sourceChatID, sourceMessageID, recipientUserID int64) (BroadcastUpstreamMessage, bool, error) {
	var upstream BroadcastUpstreamMessage
	err := s.pool.QueryRow(ctx, `SELECT id,source_operator_user_id,source_chat_id,source_message_id,
		recipient_user_id,outbox_id,telegram_message_id,created_at,sent_at
		FROM broadcast_upstream_messages
		WHERE source_chat_id=$1 AND source_message_id=$2 AND recipient_user_id=$3`,
		sourceChatID, sourceMessageID, recipientUserID).Scan(&upstream.ID, &upstream.SourceOperatorUserID,
		&upstream.SourceChatID, &upstream.SourceMessageID, &upstream.RecipientUserID, &upstream.OutboxID,
		&upstream.TelegramMessageID, &upstream.CreatedAt, &upstream.SentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BroadcastUpstreamMessage{}, false, nil
	}
	return upstream, err == nil, err
}

func (s *Store) GetBroadcastUpstreamMessageByID(ctx context.Context, id int64) (BroadcastUpstreamMessage, bool, error) {
	var upstream BroadcastUpstreamMessage
	err := s.pool.QueryRow(ctx, `SELECT id,source_operator_user_id,source_chat_id,source_message_id,
		recipient_user_id,outbox_id,telegram_message_id,created_at,sent_at
		FROM broadcast_upstream_messages WHERE id=$1`, id).Scan(&upstream.ID, &upstream.SourceOperatorUserID,
		&upstream.SourceChatID, &upstream.SourceMessageID, &upstream.RecipientUserID, &upstream.OutboxID,
		&upstream.TelegramMessageID, &upstream.CreatedAt, &upstream.SentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return BroadcastUpstreamMessage{}, false, nil
	}
	return upstream, err == nil, err
}

func (s *Store) CleanupBroadcastUpstreamMessages(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM broadcast_upstream_messages upstream
		WHERE upstream.created_at<$1
		  AND NOT EXISTS (
			SELECT 1 FROM notification_outbox outbox
			WHERE (outbox.id=upstream.outbox_id OR outbox.reply_to_upstream_id=upstream.id)
			  AND outbox.status IN ('pending','sending')
		  )`, cutoff)
	return tag.RowsAffected(), err
}
