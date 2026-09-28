package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Webhook delivery states. delivered and abandoned are terminal; pending
// rows are claimable once available_at passes.
const (
	WebhookPending   = "pending"
	WebhookDelivered = "delivered"
	WebhookAbandoned = "abandoned"
)

// WebhookDelivery is one queued outbound webhook call. Payload is the
// bounded JSON state description (never commands, never secrets); Attempts
// counts send attempts made so far.
type WebhookDelivery struct {
	ID          string
	Webhook     string
	Event       string
	Payload     string
	Attempts    int
	State       string
	AvailableAt time.Time
	CreatedAt   time.Time
	LastError   string
}

// EnqueueWebhookDelivery persists one delivery as pending BEFORE any
// network I/O. The caller owns the payload; enqueue is local and cannot
// fail from receiver outages.
func (s *Store) EnqueueWebhookDelivery(ctx context.Context, d WebhookDelivery) error {
	if d.ID == "" {
		d.ID = newID()
	}
	if d.State == "" {
		d.State = WebhookPending
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (id, webhook, event, payload, attempts, state, available_at, created_at, last_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Webhook, d.Event, d.Payload, d.Attempts, d.State, rfc3339(d.AvailableAt), rfc3339(d.CreatedAt), nullableString(d.LastError))
	if err != nil {
		return fmt.Errorf("enqueue webhook delivery: %w", err)
	}
	return nil
}

// ClaimableWebhookDeliveries returns pending deliveries whose available_at
// has passed, oldest first, up to limit.
func (s *Store) ClaimableWebhookDeliveries(ctx context.Context, now time.Time, limit int) ([]WebhookDelivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, webhook, event, payload, attempts, state, available_at, created_at, last_error
		   FROM webhook_deliveries
		  WHERE state = ? AND available_at <= ?
		  ORDER BY created_at, id LIMIT ?`,
		WebhookPending, rfc3339(now), limit)
	if err != nil {
		return nil, fmt.Errorf("claim webhook deliveries: %w", err)
	}
	defer rows.Close()
	var out []WebhookDelivery
	for rows.Next() {
		d, err := scanWebhookDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim webhook deliveries: %w", err)
	}
	return out, nil
}

// MarkWebhookDelivered records success. The state guard keeps a row that
// was abandoned (or already delivered) from being flipped by a late
// response.
func (s *Store) MarkWebhookDelivered(ctx context.Context, id string) error {
	return s.markWebhook(ctx, id,
		`UPDATE webhook_deliveries SET state = ?, attempts = attempts + 1, last_error = NULL
		  WHERE id = ? AND state = ?`,
		WebhookDelivered, id, WebhookPending)
}

// MarkWebhookRetry schedules the next attempt and records the failure.
func (s *Store) MarkWebhookRetry(ctx context.Context, id string, nextTry time.Time, failure string) error {
	return s.markWebhook(ctx, id,
		`UPDATE webhook_deliveries SET state = ?, attempts = attempts + 1, available_at = ?, last_error = ?
		  WHERE id = ? AND state = ?`,
		WebhookPending, rfc3339(nextTry), failure, id, WebhookPending)
}

// MarkWebhookAbandoned retires a delivery past its attempt cap, keeping the
// last error for diagnosis. Nothing is ever deleted by this path.
func (s *Store) MarkWebhookAbandoned(ctx context.Context, id string, failure string) error {
	return s.markWebhook(ctx, id,
		`UPDATE webhook_deliveries SET state = ?, attempts = attempts + 1, last_error = ?
		  WHERE id = ? AND state = ?`,
		WebhookAbandoned, failure, id, WebhookPending)
}

func (s *Store) markWebhook(ctx context.Context, id, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("mark webhook delivery: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("webhook delivery %s is not pending", id)
	}
	return nil
}

// PendingWebhookCount reports the backlog (diagnostics and tests).
func (s *Store) PendingWebhookCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhook_deliveries WHERE state = ?`, WebhookPending).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending webhook deliveries: %w", err)
	}
	return n, nil
}

func scanWebhookDelivery(rows *sql.Rows) (WebhookDelivery, error) {
	var d WebhookDelivery
	var available, created string
	var lastError sql.NullString
	if err := rows.Scan(&d.ID, &d.Webhook, &d.Event, &d.Payload, &d.Attempts, &d.State, &available, &created, &lastError); err != nil {
		return WebhookDelivery{}, fmt.Errorf("read webhook delivery: %w", err)
	}
	var err1, err2 error
	d.AvailableAt, err1 = time.Parse(time.RFC3339Nano, available)
	d.CreatedAt, err2 = time.Parse(time.RFC3339Nano, created)
	if err1 != nil || err2 != nil {
		return WebhookDelivery{}, fmt.Errorf("read webhook delivery: parse timestamps: %v / %v", err1, err2)
	}
	d.LastError = lastError.String
	return d, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
