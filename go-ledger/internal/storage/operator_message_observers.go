package storage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrMessageObserverInvalidIdentity = errors.New("message observer identity is invalid")
	ErrMessageObserverInvalidScope    = errors.New("message observer requires an active secondary source and a cross-primary active observer")
	ErrMessageObserverNoChannels      = errors.New("message observer requires at least one receive channel")
)

func (s *Store) UpsertOperatorMessageObserverGrant(
	ctx context.Context,
	sourceSecondaryUserID, observerPrimaryUserID int64,
	receiveBroadcast, receiveReply bool,
	grantedBy int64,
	now time.Time,
) (bool, error) {
	if sourceSecondaryUserID <= 0 || observerPrimaryUserID <= 0 || grantedBy <= 0 || sourceSecondaryUserID == observerPrimaryUserID {
		return false, ErrMessageObserverInvalidIdentity
	}
	if !receiveBroadcast && !receiveReply {
		return false, ErrMessageObserverNoChannels
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	var parentUserID int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(parent_user_id, 0)
		FROM global_operators
		WHERE user_id=$1 AND level='secondary' AND status='active'
		FOR SHARE`, sourceSecondaryUserID).Scan(&parentUserID)
	if errors.Is(err, pgx.ErrNoRows) || parentUserID <= 0 || parentUserID == observerPrimaryUserID {
		return false, ErrMessageObserverInvalidScope
	}
	if err != nil {
		return false, err
	}
	var observerExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM global_operators
		WHERE user_id=$1 AND level='primary' AND status='active'
	)`, observerPrimaryUserID).Scan(&observerExists); err != nil {
		return false, err
	}
	if !observerExists {
		return false, ErrMessageObserverInvalidScope
	}
	var oldBroadcast, oldReply, oldActive bool
	err = tx.QueryRow(ctx, `SELECT receive_broadcast, receive_reply, active
		FROM operator_message_observer_grants
		WHERE source_secondary_user_id=$1 AND observer_primary_user_id=$2
		FOR UPDATE`, sourceSecondaryUserID, observerPrimaryUserID).Scan(&oldBroadcast, &oldReply, &oldActive)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if exists && oldActive && oldBroadcast == receiveBroadcast && oldReply == receiveReply {
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operator_message_observer_grants(
			source_secondary_user_id, observer_primary_user_id,
			receive_broadcast, receive_reply, active, granted_by, granted_at,
			revoked_by, revoked_at
		) VALUES($1, $2, $3, $4, TRUE, $5, $6, NULL, NULL)
		ON CONFLICT(source_secondary_user_id, observer_primary_user_id) DO UPDATE SET
			receive_broadcast=excluded.receive_broadcast,
			receive_reply=excluded.receive_reply,
			active=TRUE,
			granted_by=excluded.granted_by,
			granted_at=excluded.granted_at,
			revoked_by=NULL,
			revoked_at=NULL`,
		sourceSecondaryUserID, observerPrimaryUserID, receiveBroadcast, receiveReply, grantedBy, now); err != nil {
		return false, err
	}
	action := "granted"
	if exists && oldActive {
		action = "updated"
	}
	if err := insertOperatorMessageObserverAudit(ctx, tx, OperatorMessageObserverAuditEvent{
		SourceSecondaryUserID: sourceSecondaryUserID,
		ObserverPrimaryUserID: observerPrimaryUserID,
		Action:                action, ReceiveBroadcast: receiveBroadcast, ReceiveReply: receiveReply,
		ActorUserID: grantedBy, CreatedAt: now,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) RevokeOperatorMessageObserverGrant(
	ctx context.Context,
	sourceSecondaryUserID, observerPrimaryUserID, revokedBy int64,
	now time.Time,
) (bool, error) {
	if sourceSecondaryUserID <= 0 || observerPrimaryUserID <= 0 || revokedBy <= 0 {
		return false, ErrMessageObserverInvalidIdentity
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	var receiveBroadcast, receiveReply bool
	err = tx.QueryRow(ctx, `UPDATE operator_message_observer_grants
		SET active=FALSE, revoked_by=$3, revoked_at=$4
		WHERE source_secondary_user_id=$1 AND observer_primary_user_id=$2 AND active=TRUE
		RETURNING receive_broadcast, receive_reply`,
		sourceSecondaryUserID, observerPrimaryUserID, revokedBy, now).Scan(&receiveBroadcast, &receiveReply)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if err := insertOperatorMessageObserverAudit(ctx, tx, OperatorMessageObserverAuditEvent{
		SourceSecondaryUserID: sourceSecondaryUserID,
		ObserverPrimaryUserID: observerPrimaryUserID,
		Action:                "revoked", ReceiveBroadcast: receiveBroadcast, ReceiveReply: receiveReply,
		ActorUserID: revokedBy, CreatedAt: now,
	}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) ListOperatorMessageObserverGrants(ctx context.Context) ([]OperatorMessageObserverGrant, error) {
	rows, err := s.pool.Query(ctx, `SELECT source_secondary_user_id, observer_primary_user_id,
		receive_broadcast, receive_reply, active, granted_by, granted_at,
		COALESCE(revoked_by, 0), revoked_at
		FROM operator_message_observer_grants
		ORDER BY source_secondary_user_id, observer_primary_user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []OperatorMessageObserverGrant
	for rows.Next() {
		var grant OperatorMessageObserverGrant
		var revokedAt pgtype.Timestamptz
		if err := rows.Scan(
			&grant.SourceSecondaryUserID, &grant.ObserverPrimaryUserID,
			&grant.ReceiveBroadcast, &grant.ReceiveReply, &grant.Active,
			&grant.GrantedBy, &grant.GrantedAt, &grant.RevokedBy, &revokedAt,
		); err != nil {
			return nil, err
		}
		if revokedAt.Valid {
			t := revokedAt.Time
			grant.RevokedAt = &t
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// ResolveOperatorMessageRecipients returns message observers only. The source
// operator remains the caller's responsibility. Host and the direct primary
// receive both channels; cross-primary observers receive only enabled channels.
func (s *Store) ResolveOperatorMessageRecipients(
	ctx context.Context,
	sourceSecondaryUserID, hostUserID int64,
) (OperatorMessageRecipients, error) {
	if sourceSecondaryUserID <= 0 || hostUserID <= 0 {
		return OperatorMessageRecipients{}, ErrMessageObserverInvalidIdentity
	}
	var parentUserID int64
	err := s.pool.QueryRow(ctx, `SELECT child.parent_user_id
		FROM global_operators child
		JOIN global_operators parent ON parent.user_id=child.parent_user_id
		WHERE child.user_id=$1 AND child.level='secondary' AND child.status='active'
		  AND parent.level='primary' AND parent.status='active'`, sourceSecondaryUserID).Scan(&parentUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return OperatorMessageRecipients{}, ErrMessageObserverInvalidScope
	}
	if err != nil {
		return OperatorMessageRecipients{}, err
	}
	broadcast := map[int64]struct{}{hostUserID: {}, parentUserID: {}}
	reply := map[int64]struct{}{hostUserID: {}, parentUserID: {}}
	rows, err := s.pool.Query(ctx, `SELECT grant_row.observer_primary_user_id,
		grant_row.receive_broadcast, grant_row.receive_reply
		FROM operator_message_observer_grants grant_row
		JOIN global_operators observer ON observer.user_id=grant_row.observer_primary_user_id
		WHERE grant_row.source_secondary_user_id=$1 AND grant_row.active=TRUE
		  AND observer.level='primary' AND observer.status='active'`, sourceSecondaryUserID)
	if err != nil {
		return OperatorMessageRecipients{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var observerUserID int64
		var receiveBroadcast, receiveReply bool
		if err := rows.Scan(&observerUserID, &receiveBroadcast, &receiveReply); err != nil {
			return OperatorMessageRecipients{}, err
		}
		if receiveBroadcast {
			broadcast[observerUserID] = struct{}{}
		}
		if receiveReply {
			reply[observerUserID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return OperatorMessageRecipients{}, err
	}
	return OperatorMessageRecipients{
		Broadcast: sortedRecipientIDs(broadcast),
		Reply:     sortedRecipientIDs(reply),
	}, nil
}

func (s *Store) ListOperatorMessageObserverAuditEvents(ctx context.Context, limit int) ([]OperatorMessageObserverAuditEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, source_secondary_user_id, observer_primary_user_id,
		action, receive_broadcast, receive_reply, actor_user_id, created_at
		FROM operator_message_observer_audit_events
		ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []OperatorMessageObserverAuditEvent
	for rows.Next() {
		var event OperatorMessageObserverAuditEvent
		if err := rows.Scan(
			&event.ID, &event.SourceSecondaryUserID, &event.ObserverPrimaryUserID,
			&event.Action, &event.ReceiveBroadcast, &event.ReceiveReply,
			&event.ActorUserID, &event.CreatedAt,
		); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func insertOperatorMessageObserverAudit(ctx context.Context, tx pgx.Tx, event OperatorMessageObserverAuditEvent) error {
	_, err := tx.Exec(ctx, `INSERT INTO operator_message_observer_audit_events(
		source_secondary_user_id, observer_primary_user_id, action,
		receive_broadcast, receive_reply, actor_user_id, created_at
	) VALUES($1, $2, $3, $4, $5, $6, $7)`,
		event.SourceSecondaryUserID, event.ObserverPrimaryUserID, event.Action,
		event.ReceiveBroadcast, event.ReceiveReply, event.ActorUserID, event.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert operator message observer audit: %w", err)
	}
	return nil
}

func sortedRecipientIDs(ids map[int64]struct{}) []int64 {
	result := make([]int64, 0, len(ids))
	for userID := range ids {
		if userID > 0 {
			result = append(result, userID)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
