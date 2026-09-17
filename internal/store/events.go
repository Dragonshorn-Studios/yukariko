package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Observation kinds recorded separately from deployed versions.
const (
	KindGitSHA = "git_sha"
	KindDigest = "digest"
)

// RecordObservation stores the latest observed source version for an app.
// Observations describe what was seen; they never imply deployment.
func (s *Store) RecordObservation(ctx context.Context, appID, kind, value string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO observations (app_id, kind, value, observed_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(app_id, kind) DO UPDATE SET
		   value = excluded.value,
		   observed_at = excluded.observed_at`,
		appID, kind, value, rfc3339(at))
	if err != nil {
		return fmt.Errorf("record observation: %w", err)
	}
	return nil
}

// ObservedVersion returns the latest observed version of the given kind.
func (s *Store) ObservedVersion(ctx context.Context, appID, kind string) (string, time.Time, bool, error) {
	var value, at string
	err := s.db.QueryRowContext(ctx,
		`SELECT value, observed_at FROM observations WHERE app_id = ? AND kind = ?`,
		appID, kind).Scan(&value, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("read observation: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("parse observed_at: %w", err)
	}
	return value, parsed, true, nil
}

// Event levels.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Event is one structured history entry. Data is a JSON object or empty;
// it must never carry secret values — callers redact before recording.
type Event struct {
	ID      int64
	Time    time.Time
	AppID   string
	Level   string
	Kind    string
	Message string
	Data    string
}

// RecordEvent appends one event to the durable history.
func (s *Store) RecordEvent(ctx context.Context, e Event) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (ts, app_id, level, kind, message, data) VALUES (?, ?, ?, ?, ?, ?)`,
		rfc3339(e.Time), e.AppID, e.Level, e.Kind, e.Message, e.Data)
	if err != nil {
		return 0, fmt.Errorf("record event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record event: id: %w", err)
	}
	return id, nil
}

// EventsQuery bounds and filters event reads.
type EventsQuery struct {
	AppID string // empty = all apps
	Level string // empty = all levels
	Limit int    // required; hard-capped
}

// maxEventLimit is the hard cap applied to every event read regardless of
// the requested limit.
const maxEventLimit = 1000

// Events returns up to Limit events, newest first.
func (s *Store) Events(ctx context.Context, q EventsQuery) ([]Event, error) {
	limit := q.Limit
	if limit <= 0 || limit > maxEventLimit {
		limit = maxEventLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, app_id, level, kind, message, data FROM events
		 WHERE (? = '' OR app_id = ?) AND (? = '' OR level = ?)
		 ORDER BY id DESC LIMIT ?`,
		q.AppID, q.AppID, q.Level, q.Level, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts, appID, data sql.NullString
		if err := rows.Scan(&e.ID, &ts, &appID, &e.Level, &e.Kind, &e.Message, &data); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts.String)
		e.AppID = appID.String
		e.Data = data.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// CommandRun is bounded, secret-free metadata about one executed command.
// Excerpts are caller-redacted before they reach the store.
type CommandRun struct {
	ID            int64
	DeploymentID  string
	AppID         string
	Name          string
	Argv          string // argv joined for display; not a shell string
	Dir           string
	Status        string // success | failed | timeout | cancelled
	ExitCode      *int
	StartedAt     time.Time
	EndedAt       time.Time
	StdoutExcerpt string
	StderrExcerpt string
	StdoutBytes   int64
	StderrBytes   int64
	Truncated     bool
}

// RecordCommandRun persists one command summary.
func (s *Store) RecordCommandRun(ctx context.Context, r CommandRun) error {
	truncated := 0
	if r.Truncated {
		truncated = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO command_runs (deployment_id, app_id, name, argv, dir, status, exit_code,
		   started_at, ended_at, stdout_excerpt, stderr_excerpt, stdout_bytes, stderr_bytes, truncated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DeploymentID, r.AppID, r.Name, r.Argv, r.Dir, r.Status, r.ExitCode,
		rfc3339(r.StartedAt), rfc3339(r.EndedAt), r.StdoutExcerpt, r.StderrExcerpt,
		r.StdoutBytes, r.StderrBytes, truncated)
	if err != nil {
		return fmt.Errorf("record command run: %w", err)
	}
	return nil
}

// CommandRunsForDeployment lists the command summaries of one deployment.
func (s *Store) CommandRunsForDeployment(ctx context.Context, deploymentID string) ([]CommandRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, deployment_id, app_id, name, argv, dir, status, exit_code,
		        started_at, ended_at, stdout_excerpt, stderr_excerpt, stdout_bytes, stderr_bytes, truncated
		 FROM command_runs WHERE deployment_id = ? ORDER BY id`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("list command runs: %w", err)
	}
	defer rows.Close()
	var out []CommandRun
	for rows.Next() {
		var r CommandRun
		var depID, appID sql.NullString
		var exit sql.NullInt64
		var started, ended sql.NullString
		var stdout, stderr sql.NullString
		var truncated int
		if err := rows.Scan(&r.ID, &depID, &appID, &r.Name, &r.Argv, &r.Dir, &r.Status, &exit,
			&started, &ended, &stdout, &stderr, &r.StdoutBytes, &r.StderrBytes, &truncated); err != nil {
			return nil, fmt.Errorf("scan command run: %w", err)
		}
		r.DeploymentID = depID.String
		r.AppID = appID.String
		if exit.Valid {
			v := int(exit.Int64)
			r.ExitCode = &v
		}
		r.StartedAt, _ = time.Parse(time.RFC3339Nano, started.String)
		if t, err := time.Parse(time.RFC3339Nano, ended.String); err == nil {
			r.EndedAt = t
		}
		r.StdoutExcerpt = stdout.String
		r.StderrExcerpt = stderr.String
		r.Truncated = truncated != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
