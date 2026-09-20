package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Deployment statuses.
const (
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
)

// ErrDeploymentNotRunning is returned when a deployment is expected to be in
// the running state but is not (already finished, or never begun).
var ErrDeploymentNotRunning = errors.New("deployment is not running")

// ErrDeploymentInProgress is returned by BeginDeployment when another pass
// still holds the app's single open deployment row — the cross-process
// counterpart of the scheduler's per-app lock.
var ErrDeploymentInProgress = errors.New("deployment already in progress")

// BeginDeploymentParams describes one update attempt.
type BeginDeploymentParams struct {
	AppID       string
	Cause       string // e.g. "scheduled", "manual"
	FromVersion string
	ToVersion   string
	At          time.Time
}

// BeginDeployment records a new running deployment and returns its ID. At
// most one running row may exist per app (partial unique index): a second
// begin while one is open fails with ErrDeploymentInProgress, so manual
// and scheduled passes across processes cannot overlap silently.
func (s *Store) BeginDeployment(ctx context.Context, p BeginDeploymentParams) (string, error) {
	id := newID()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO deployments (id, app_id, cause, from_version, to_version, status, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, p.AppID, p.Cause, p.FromVersion, p.ToVersion, StatusRunning, rfc3339(p.At))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return "", fmt.Errorf("begin deployment for %s: %w", p.AppID, ErrDeploymentInProgress)
		}
		return "", fmt.Errorf("begin deployment: %w", err)
	}
	return id, nil
}

// OpenDeployment returns the app's currently running deployment, if any.
func (s *Store) OpenDeployment(ctx context.Context, appID string) (Deployment, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, cause, from_version, to_version, status, started_at, ended_at, error
		 FROM deployments WHERE app_id = ? AND status = ?
		 ORDER BY started_at DESC LIMIT 1`,
		appID, StatusRunning)
	if err != nil {
		return Deployment{}, false, fmt.Errorf("read open deployment: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Deployment{}, false, rows.Err()
	}
	var d Deployment
	var started, ended sql.NullString
	var failure sql.NullString
	if err := rows.Scan(&d.ID, &d.AppID, &d.Cause, &d.FromVersion, &d.ToVersion,
		&d.Status, &started, &ended, &failure); err != nil {
		return Deployment{}, false, fmt.Errorf("scan open deployment: %w", err)
	}
	d.StartedAt, _ = time.Parse(time.RFC3339Nano, started.String)
	d.Error = failure.String
	return d, true, rows.Err()
}

// ReapStaleDeployments interrupts running deployments older than maxAge —
// rows whose owning pass died with its process — and returns how many.
// No live pass can outlive its budget, so a generous maxAge is safe.
func (s *Store) ReapStaleDeployments(ctx context.Context, maxAge time.Duration, reason string, now time.Time) (int, error) {
	cutoff := now.Add(-maxAge)
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, ended_at = ?, error = ?
		 WHERE status = ? AND started_at < ?`,
		StatusInterrupted, rfc3339(now), reason, StatusRunning, rfc3339(cutoff))
	if err != nil {
		return 0, fmt.Errorf("reap stale deployments: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ReleaseRunningDeployment deletes a running deployment row. Restart uses
// this to hold the unique-index lock without leaving a deployment record
// or advancing the checkpoint. It is a no-op when the row is already gone.
func (s *Store) ReleaseRunningDeployment(ctx context.Context, deploymentID string) error {
	if deploymentID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM deployments WHERE id = ? AND status = ?`,
		deploymentID, StatusRunning)
	if err != nil {
		return fmt.Errorf("release running deployment: %w", err)
	}
	return nil
}

// FinishDeployment moves a running deployment to failed or interrupted. It
// never touches the deployed version: only CommitDeploymentSuccess can
// advance it.
func (s *Store) FinishDeployment(ctx context.Context, deploymentID, status string, failure string, at time.Time) error {
	if status != StatusFailed && status != StatusInterrupted {
		return fmt.Errorf("FinishDeployment status must be %q or %q, got %q", StatusFailed, StatusInterrupted, status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, ended_at = ?, error = ?
		 WHERE id = ? AND status = ?`,
		status, rfc3339(at), failure, deploymentID, StatusRunning)
	if err != nil {
		return fmt.Errorf("finish deployment: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrDeploymentNotRunning, deploymentID)
	}
	return nil
}

// CommitDeploymentSuccess is the only path that advances an app's deployed
// version. It atomically verifies the deployment is still running, marks it
// succeeded, and records the deployed version. If any step fails the whole
// transaction rolls back, so a crash or error can never leave a version
// recorded as deployed without a succeeded deployment — or vice versa.
func (s *Store) CommitDeploymentSuccess(ctx context.Context, appID, kind, value, deploymentID string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("commit deployment success: %w", err)
	}
	defer tx.Rollback()

	var running bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM deployments WHERE id = ? AND status = ?)`,
		deploymentID, StatusRunning).Scan(&running); err != nil {
		return fmt.Errorf("commit deployment success: check deployment: %w", err)
	}
	if !running {
		return fmt.Errorf("%w: %s", ErrDeploymentNotRunning, deploymentID)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE deployments SET status = ?, ended_at = ?, error = NULL WHERE id = ?`,
		StatusSucceeded, rfc3339(at), deploymentID); err != nil {
		return fmt.Errorf("commit deployment success: update deployment: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deployed_versions (app_id, kind, value, deployment_id, deployed_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(app_id) DO UPDATE SET
		   kind = excluded.kind,
		   value = excluded.value,
		   deployment_id = excluded.deployment_id,
		   deployed_at = excluded.deployed_at`,
		appID, kind, value, deploymentID, rfc3339(at)); err != nil {
		return fmt.Errorf("commit deployment success: record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit deployment success: %w", err)
	}
	return nil
}

// DeployedVersion returns the last successfully deployed version of the
// given kind for an app, and whether one exists.
func (s *Store) DeployedVersion(ctx context.Context, appID, kind string) (string, time.Time, bool, error) {
	var value, at string
	err := s.db.QueryRowContext(ctx,
		`SELECT value, deployed_at FROM deployed_versions WHERE app_id = ? AND kind = ?`,
		appID, kind).Scan(&value, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("read deployed version: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("parse deployed_at: %w", err)
	}
	return value, parsed, true, nil
}

// Deployment is one immutable update attempt.
type Deployment struct {
	ID          string
	AppID       string
	Cause       string
	FromVersion string
	ToVersion   string
	Status      string
	StartedAt   time.Time
	EndedAt     time.Time
	Ended       bool
	Error       string
}

// RecentDeployments returns up to limit deployments, newest first.
func (s *Store) RecentDeployments(ctx context.Context, appID string, limit int) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, app_id, cause, from_version, to_version, status, started_at, ended_at, error
		 FROM deployments WHERE (? = '' OR app_id = ?)
		 ORDER BY started_at DESC, id DESC LIMIT ?`,
		appID, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		var started, ended sql.NullString
		var failure sql.NullString
		if err := rows.Scan(&d.ID, &d.AppID, &d.Cause, &d.FromVersion, &d.ToVersion,
			&d.Status, &started, &ended, &failure); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		d.StartedAt, _ = time.Parse(time.RFC3339Nano, started.String)
		if ended.Valid {
			if t, err := time.Parse(time.RFC3339Nano, ended.String); err == nil {
				d.Ended = true
				d.EndedAt = t
			}
		}
		d.Error = failure.String
		out = append(out, d)
	}
	return out, rows.Err()
}
