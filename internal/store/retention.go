package store

import (
	"context"
	"fmt"
	"time"
)

// RetentionPolicy bounds history growth. It never applies to the outbound
// queue or replay markers: pending reports and replay-window data are only
// governed by their own delivery/replay lifecycles.
type RetentionPolicy struct {
	EventsDays      int
	HealthDays      int
	DeploymentsDays int
}

// CleanupResult reports how many rows each retention rule removed.
type CleanupResult struct {
	Events      int64
	Health      int64
	Deployments int64
}

// Cleanup deletes history older than the policy windows, measured from now.
// Running deployments and every outbound queue row are always preserved.
func (s *Store) Cleanup(ctx context.Context, p RetentionPolicy, now time.Time) (CleanupResult, error) {
	var result CleanupResult

	eventsCutoff := now.AddDate(0, 0, -p.EventsDays)
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE ts < ?`, rfc3339(eventsCutoff))
	if err != nil {
		return result, fmt.Errorf("cleanup events: %w", err)
	}
	result.Events, _ = res.RowsAffected()

	healthCutoff := now.AddDate(0, 0, -p.HealthDays)
	res, err = s.db.ExecContext(ctx, `DELETE FROM health_samples WHERE checked_at < ?`, rfc3339(healthCutoff))
	if err != nil {
		return result, fmt.Errorf("cleanup health samples: %w", err)
	}
	result.Health, _ = res.RowsAffected()

	deployCutoff := now.AddDate(0, 0, -p.DeploymentsDays)
	res, err = s.db.ExecContext(ctx,
		`DELETE FROM deployments WHERE status != 'running' AND started_at < ?`,
		rfc3339(deployCutoff))
	if err != nil {
		return result, fmt.Errorf("cleanup deployments: %w", err)
	}
	result.Deployments, _ = res.RowsAffected()

	return result, nil
}
