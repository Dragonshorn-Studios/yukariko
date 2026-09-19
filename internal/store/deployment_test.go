package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// At most one open deployment per app, enforced by the database so
// separate processes cannot race past an in-memory check.
func TestSingleOpenDeployment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	now := time.Now()

	first, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "app", Cause: "scheduled", At: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "app", Cause: "manual", At: now.Add(time.Second)}); !errors.Is(err, ErrDeploymentInProgress) {
		t.Fatalf("second begin err = %v, want ErrDeploymentInProgress", err)
	}
	open, ok, err := s.OpenDeployment(ctx, "app")
	if err != nil || !ok || open.ID != first || open.Cause != "scheduled" {
		t.Fatalf("open = %+v ok=%v err=%v, want the first row", open, ok, err)
	}
	// Other apps are unaffected.
	if _, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "other", At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeployment(ctx, first, StatusFailed, "boom", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.OpenDeployment(ctx, "app"); ok {
		t.Fatal("open deployment survived its finish")
	}
	again, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "app", Cause: "manual", At: now.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("begin after finish = %v", err)
	}
	_ = again
}

// Rows older than the threshold are interrupted; fresh ones untouched.
func TestReapStaleDeployments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	now := time.Now()
	old, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "a", At: now.Add(-2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "b", At: now.Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.ReapStaleDeployments(ctx, time.Hour, "stale: test", now)
	if err != nil || n != 1 {
		t.Fatalf("reap = %d, %v; want 1", n, err)
	}
	if _, ok, _ := s.OpenDeployment(ctx, "a"); ok {
		t.Fatal("stale row survived the reap")
	}
	d, _ := s.RecentDeployments(ctx, "a", 1)
	if len(d) != 1 || d[0].Status != StatusInterrupted || d[0].ID != old || d[0].Error != "stale: test" {
		t.Fatalf("reaped row = %+v", d)
	}
	if _, ok, _ := s.OpenDeployment(ctx, "b"); !ok {
		t.Fatalf("fresh row %s was reaped", fresh)
	}
}
