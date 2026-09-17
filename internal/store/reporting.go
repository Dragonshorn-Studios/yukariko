package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Outbound event states.
const (
	OutboundPending   = "pending"
	OutboundDelivered = "delivered"
)

// OutboundEvent is one durable message in the outbound reporting queue. The
// queue and its primitives are reserved for issue #18; persisting them now
// lets retention and schema settle early.
type OutboundEvent struct {
	ID          string
	CreatedAt   time.Time
	AvailableAt time.Time
	Attempts    int
	State       string
	CoalesceKey string
	Kind        string
	Payload     string // JSON; status only, never commands or secrets
	LastError   string
}

// EnqueueOutboundEvent inserts one event into the durable outbox before any
// delivery is attempted.
func (s *Store) EnqueueOutboundEvent(ctx context.Context, e OutboundEvent) error {
	if e.ID == "" {
		e.ID = newID()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO outbound_events (id, created_at, available_at, attempts, state, coalesce_key, kind, payload, last_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, rfc3339(e.CreatedAt), rfc3339(e.AvailableAt), e.Attempts, OutboundPending,
		e.CoalesceKey, e.Kind, e.Payload, e.LastError)
	if err != nil {
		return fmt.Errorf("enqueue outbound event: %w", err)
	}
	return nil
}

// ClaimableOutboundEvents returns pending events whose available_at has
// passed, oldest first.
func (s *Store) ClaimableOutboundEvents(ctx context.Context, now time.Time, limit int) ([]OutboundEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, available_at, attempts, state, coalesce_key, kind, payload, last_error
		 FROM outbound_events WHERE state = ? AND available_at <= ?
		 ORDER BY created_at, id LIMIT ?`,
		OutboundPending, rfc3339(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list outbound events: %w", err)
	}
	defer rows.Close()
	return scanOutbound(rows)
}

// RecordDeliveryAttempt appends one delivery attempt result.
func (s *Store) RecordDeliveryAttempt(ctx context.Context, eventID string, at time.Time, result string, statusCode *int, failure string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO delivery_attempts (event_id, attempted_at, result, status_code, error)
		 VALUES (?, ?, ?, ?, ?)`,
		eventID, rfc3339(at), result, statusCode, failure)
	if err != nil {
		return fmt.Errorf("record delivery attempt: %w", err)
	}
	return nil
}

// MarkOutboundDelivered flags an event as delivered and bumps its attempt
// counter. Delivering twice is reported, never silently duplicated: use
// MarkOutboundDelivered only after an acknowledged 2xx.
func (s *Store) MarkOutboundDelivered(ctx context.Context, eventID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE outbound_events SET state = ?, attempts = attempts + 1, last_error = NULL
		 WHERE id = ? AND state = ?`,
		OutboundDelivered, eventID, OutboundPending)
	if err != nil {
		return fmt.Errorf("mark outbound delivered: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("outbound event %s is not pending", eventID)
	}
	return nil
}

// MarkOutboundRetry records a failed attempt and schedules the next try.
func (s *Store) MarkOutboundRetry(ctx context.Context, eventID string, nextTry time.Time, failure string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE outbound_events SET attempts = attempts + 1, available_at = ?, last_error = ?
		 WHERE id = ? AND state = ?`,
		rfc3339(nextTry), failure, eventID, OutboundPending)
	if err != nil {
		return fmt.Errorf("mark outbound retry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("outbound event %s is not pending", eventID)
	}
	return nil
}

// CoalesceOutboundEvent atomically supersedes the pending event with the
// same coalesce key (e.g. an outdated heartbeat) or inserts a new one. The
// surviving row keeps its original ID so delivery attempts keep their
// foreign key and receivers never see a mutated ID. It returns the ID that
// is now queued.
func (s *Store) CoalesceOutboundEvent(ctx context.Context, e OutboundEvent) (string, error) {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.CoalesceKey == "" {
		return e.ID, s.EnqueueOutboundEvent(ctx, e)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("coalesce outbound event: %w", err)
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM outbound_events WHERE coalesce_key = ? AND state = ?`,
		e.CoalesceKey, OutboundPending).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("coalesce outbound event: find: %w", err)
	}
	if existing != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbound_events SET created_at = ?, available_at = ?, payload = ?, last_error = NULL
			 WHERE id = ?`,
			rfc3339(e.CreatedAt), rfc3339(e.AvailableAt), e.Payload, existing); err != nil {
			return "", fmt.Errorf("coalesce outbound event: replace: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO outbound_events (id, created_at, available_at, attempts, state, coalesce_key, kind, payload, last_error)
			 VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?)`,
			e.ID, rfc3339(e.CreatedAt), rfc3339(e.AvailableAt), OutboundPending,
			e.CoalesceKey, e.Kind, e.Payload, e.LastError); err != nil {
			return "", fmt.Errorf("coalesce outbound event: insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("coalesce outbound event: %w", err)
	}
	if existing != "" {
		return existing, nil
	}
	return e.ID, nil
}

// PendingOutboundCount returns how many events still await delivery.
func (s *Store) PendingOutboundCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbound_events WHERE state = ?`, OutboundPending).Scan(&n); err != nil {
		return 0, fmt.Errorf("count outbound pending: %w", err)
	}
	return n, nil
}

// LastDelivery returns the time of the last acknowledged delivery, if any.
func (s *Store) LastDelivery(ctx context.Context) (time.Time, bool, error) {
	var at string
	err := s.db.QueryRowContext(ctx,
		`SELECT attempted_at FROM delivery_attempts WHERE result = 'delivered' ORDER BY id DESC LIMIT 1`).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read last delivery: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse last delivery: %w", err)
	}
	return t, true, nil
}

// Inbound event replay protection: returns false when the (host, event) pair
// is new, true when it was already accepted inside the replay window.
func (s *Store) SeenInboundEvent(ctx context.Context, hostID, eventID string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO inbound_event_ids (host_id, event_id, received_at) VALUES (?, ?, ?)
		 ON CONFLICT(host_id, event_id) DO NOTHING`,
		hostID, eventID, rfc3339(at))
	if err != nil {
		return false, fmt.Errorf("remember inbound event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("remember inbound event: %w", err)
	}
	return n == 0, nil
}

// PurgeInboundEventIDs forgets replay markers older than the cutoff. It is
// the only deletion path for replay data and is bounded by the configured
// replay window, never by retention.
func (s *Store) PurgeInboundEventIDs(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM inbound_event_ids WHERE received_at < ?`, rfc3339(cutoff))
	if err != nil {
		return 0, fmt.Errorf("purge inbound event ids: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge inbound event ids: %w", err)
	}
	return n, nil
}

// TouchRemoteHost records that a peer was seen.
func (s *Store) TouchRemoteHost(ctx context.Context, hostID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO remote_hosts (host_id, created_at, last_seen_at) VALUES (?, ?, ?)
		 ON CONFLICT(host_id) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		hostID, rfc3339(at), rfc3339(at))
	if err != nil {
		return fmt.Errorf("touch remote host: %w", err)
	}
	return nil
}

// RemoteHost is a peer's presence record.
type RemoteHost struct {
	HostID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// RemoteHosts lists all known peers.
func (s *Store) RemoteHosts(ctx context.Context) ([]RemoteHost, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_id, created_at, last_seen_at FROM remote_hosts ORDER BY host_id`)
	if err != nil {
		return nil, fmt.Errorf("list remote hosts: %w", err)
	}
	defer rows.Close()
	var out []RemoteHost
	for rows.Next() {
		var h RemoteHost
		var created, seen string
		if err := rows.Scan(&h.HostID, &created, &seen); err != nil {
			return nil, fmt.Errorf("scan remote host: %w", err)
		}
		h.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		h.LastSeenAt, _ = time.Parse(time.RFC3339Nano, seen)
		out = append(out, h)
	}
	return out, rows.Err()
}

// UpsertRemoteState stores the latest reported state blob for one peer app.
func (s *Store) UpsertRemoteState(ctx context.Context, hostID, appID, data string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO remote_state (host_id, app_id, data, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(host_id, app_id) DO UPDATE SET
		   data = excluded.data,
		   updated_at = excluded.updated_at`,
		hostID, appID, data, rfc3339(at))
	if err != nil {
		return fmt.Errorf("upsert remote state: %w", err)
	}
	return nil
}

// RemoteState is one peer app's latest reported state.
type RemoteState struct {
	HostID    string
	AppID     string
	Data      string
	UpdatedAt time.Time
}

// RemoteStates lists peer-reported state, optionally for one host.
func (s *Store) RemoteStates(ctx context.Context, hostID string) ([]RemoteState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_id, app_id, data, updated_at FROM remote_state
		 WHERE (? = '' OR host_id = ?) ORDER BY host_id, app_id`, hostID, hostID)
	if err != nil {
		return nil, fmt.Errorf("list remote state: %w", err)
	}
	defer rows.Close()
	var out []RemoteState
	for rows.Next() {
		var r RemoteState
		var updated string
		if err := rows.Scan(&r.HostID, &r.AppID, &r.Data, &updated); err != nil {
			return nil, fmt.Errorf("scan remote state: %w", err)
		}
		r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanOutbound(rows *sql.Rows) ([]OutboundEvent, error) {
	var out []OutboundEvent
	for rows.Next() {
		var e OutboundEvent
		var created, available string
		var coalesce, failure sql.NullString
		if err := rows.Scan(&e.ID, &created, &available, &e.Attempts, &e.State, &coalesce, &e.Kind, &e.Payload, &failure); err != nil {
			return nil, fmt.Errorf("scan outbound event: %w", err)
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		e.AvailableAt, _ = time.Parse(time.RFC3339Nano, available)
		e.CoalesceKey = coalesce.String
		e.LastError = failure.String
		out = append(out, e)
	}
	return out, rows.Err()
}
