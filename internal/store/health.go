package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RecordHealthSample stores one health observation and updates the current
// health projection for (app, check kind).
func (s *Store) RecordHealthSample(ctx context.Context, appID, checkKind, state, reason string, latency *time.Duration, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record health sample: %w", err)
	}
	defer tx.Rollback()

	var latencyMs any
	if latency != nil {
		latencyMs = latency.Milliseconds()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO health_samples (app_id, check_kind, state, reason, latency_ms, checked_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		appID, checkKind, state, reason, latencyMs, rfc3339(at)); err != nil {
		return fmt.Errorf("record health sample: insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO health_current (app_id, check_kind, state, reason, checked_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(app_id, check_kind) DO UPDATE SET
		   state = excluded.state,
		   reason = excluded.reason,
		   checked_at = excluded.checked_at,
		   updated_at = excluded.updated_at`,
		appID, checkKind, state, reason, rfc3339(at), rfc3339(at)); err != nil {
		return fmt.Errorf("record health sample: project: %w", err)
	}
	return tx.Commit()
}

// Health is one projected health state.
type Health struct {
	AppID     string
	CheckKind string
	State     string
	Reason    string
	CheckedAt time.Time
}

// CurrentHealth returns the projected health for every check of one app.
func (s *Store) CurrentHealth(ctx context.Context, appID string) ([]Health, error) {
	return s.queryHealth(ctx,
		`SELECT app_id, check_kind, state, reason, checked_at FROM health_current
		 WHERE app_id = ? ORDER BY check_kind`, appID)
}

// CurrentHealthAll returns the projected health for every app and check.
func (s *Store) CurrentHealthAll(ctx context.Context) ([]Health, error) {
	return s.queryHealth(ctx,
		`SELECT app_id, check_kind, state, reason, checked_at FROM health_current
		 ORDER BY app_id, check_kind`, "")
}

func (s *Store) queryHealth(ctx context.Context, query, appID string) ([]Health, error) {
	rows, err := s.db.QueryContext(ctx, query, appID)
	if err != nil {
		return nil, fmt.Errorf("read health: %w", err)
	}
	defer rows.Close()
	var out []Health
	for rows.Next() {
		var h Health
		var reason sql.NullString
		var checked string
		if err := rows.Scan(&h.AppID, &h.CheckKind, &h.State, &reason, &checked); err != nil {
			return nil, fmt.Errorf("scan health: %w", err)
		}
		h.Reason = reason.String
		h.CheckedAt, _ = time.Parse(time.RFC3339Nano, checked)
		out = append(out, h)
	}
	return out, rows.Err()
}

// HealthAt returns one projected check, if any.
func (s *Store) HealthAt(ctx context.Context, appID, checkKind string) (Health, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT app_id, check_kind, state, reason, checked_at FROM health_current
		 WHERE app_id = ? AND check_kind = ?`, appID, checkKind)
	if err != nil {
		return Health{}, false, fmt.Errorf("read health: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Health{}, false, rows.Err()
	}
	var h Health
	var reason sql.NullString
	var checked string
	if err := rows.Scan(&h.AppID, &h.CheckKind, &h.State, &reason, &checked); err != nil {
		return Health{}, false, fmt.Errorf("scan health: %w", err)
	}
	h.Reason = reason.String
	h.CheckedAt, _ = time.Parse(time.RFC3339Nano, checked)
	return h, true, nil
}
