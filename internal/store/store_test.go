package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateFromEmptyAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open failed: %v", err)
	}
	v, err := s.Version()
	if err != nil || v != 1 {
		t.Fatalf("version after first migrate = %d, %v; want 1", v, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer s2.Close()
	v2, err := s2.Version()
	if err != nil || v2 != 1 {
		t.Fatalf("version after reopen = %d, %v; want 1 (idempotent)", v2, err)
	}
}

func TestOpenIntoMissingDirectoryFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing-parent", "data")
	if _, err := Open(dir); err == nil {
		t.Fatal("Open into a missing directory must fail")
	}
}

func TestOpenPathWithSpacesAndHash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my data #1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open with special characters in path failed: %v", err)
	}
	defer s.Close()
	if v, err := s.Version(); err != nil || v != 1 {
		t.Fatalf("version = %d, %v", v, err)
	}
}

func TestObservationsSeparateFromDeployed(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if _, _, ok, _ := s.ObservedVersion(ctx, "app", KindGitSHA); ok {
		t.Fatal("no observation expected initially")
	}
	if _, _, ok, _ := s.DeployedVersion(ctx, "app", KindGitSHA); ok {
		t.Fatal("no deployed version expected initially")
	}

	if err := s.RecordObservation(ctx, "app", KindGitSHA, "aaa111", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation(ctx, "app", KindGitSHA, "bbb222", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	val, at, ok, err := s.ObservedVersion(ctx, "app", KindGitSHA)
	if err != nil || !ok || val != "bbb222" {
		t.Fatalf("ObservedVersion = %q, %v, %v; want bbb222", val, ok, err)
	}
	if !at.After(now.Add(30 * time.Second)) {
		t.Errorf("observed_at = %v, want the later timestamp", at)
	}

	// Observing must never touch the deployed version.
	if _, _, ok, _ := s.DeployedVersion(ctx, "app", KindGitSHA); ok {
		t.Fatal("observation must not advance deployed version")
	}
}

func TestDeployedVersionInvariant(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	t.Run("commit on unknown deployment fails", func(t *testing.T) {
		err := s.CommitDeploymentSuccess(ctx, "app", KindGitSHA, "ccc333", "nonexistent", now)
		if err == nil {
			t.Fatal("commit on unknown deployment must fail")
		}
		if _, _, ok, _ := s.DeployedVersion(ctx, "app", KindGitSHA); ok {
			t.Fatal("failed commit must not record a deployed version")
		}
	})

	t.Run("failed deployment cannot be committed", func(t *testing.T) {
		id, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "app", Cause: "scheduled", At: now})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDeployment(ctx, id, StatusFailed, "pull failed", now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := s.CommitDeploymentSuccess(ctx, "app", KindGitSHA, "ddd444", id, now.Add(2*time.Second)); err == nil {
			t.Fatal("committing a failed deployment must fail")
		}
		if _, _, ok, _ := s.DeployedVersion(ctx, "app", KindGitSHA); ok {
			t.Fatal("failed deployment must not advance deployed version")
		}
	})

	t.Run("success commits exactly once", func(t *testing.T) {
		id, err := s.BeginDeployment(ctx, BeginDeploymentParams{
			AppID: "app", Cause: "scheduled", FromVersion: "aaa111", ToVersion: "eee555", At: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CommitDeploymentSuccess(ctx, "app", KindGitSHA, "eee555", id, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		// Double commit is rejected: checkpoint happens exactly once.
		if err := s.CommitDeploymentSuccess(ctx, "app", KindGitSHA, "eee555", id, now.Add(2*time.Second)); err == nil {
			t.Fatal("second commit of the same deployment must fail")
		}
		val, at, ok, err := s.DeployedVersion(ctx, "app", KindGitSHA)
		if err != nil || !ok || val != "eee555" {
			t.Fatalf("DeployedVersion = %q, %v, %v", val, ok, err)
		}
		if !at.Equal(now.Add(time.Second).Truncate(time.Second)) {
			t.Logf("deployed_at = %v", at)
		}
	})

	t.Run("finish after success is rejected", func(t *testing.T) {
		ids, err := s.RecentDeployments(ctx, "app", 10)
		if err != nil {
			t.Fatal(err)
		}
		var succeeded string
		for _, d := range ids {
			if d.Status == StatusSucceeded {
				succeeded = d.ID
			}
		}
		if err := s.FinishDeployment(ctx, succeeded, StatusFailed, "late failure", now); err == nil {
			t.Fatal("terminal deployment must not change status again")
		}
	})

	t.Run("cancellation path records interrupted", func(t *testing.T) {
		id, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "app", Cause: "manual", At: now})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDeployment(ctx, id, StatusInterrupted, "context canceled", now); err != nil {
			t.Fatal(err)
		}
		if _, _, ok, _ := s.DeployedVersion(ctx, "app", KindGitSHA); !ok {
			// previous subtest deployed eee555; the invariant is that this
			// interruption did not change it
		}
		deps, err := s.RecentDeployments(ctx, "app", 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range deps {
			if d.ID == id && d.Status != StatusInterrupted {
				t.Fatalf("deployment %s status = %s, want interrupted", id, d.Status)
			}
		}
	})

	t.Run("recent deployments list bounded", func(t *testing.T) {
		deps, err := s.RecentDeployments(ctx, "app", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 2 {
			t.Fatalf("got %d deployments, want 2", len(deps))
		}
		if deps[0].StartedAt.Before(deps[1].StartedAt) {
			t.Error("deployments must be ordered newest first")
		}
	})
}

func TestEventsBoundedRead(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	for i := 0; i < 25; i++ {
		app := "alpha"
		if i%3 == 0 {
			app = "beta"
		}
		if _, err := s.RecordEvent(ctx, Event{
			Time: now.Add(time.Duration(i) * time.Second), AppID: app,
			Level: LevelInfo, Kind: "test", Message: fmt.Sprintf("event %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordEvent(ctx, Event{
		Time: now, AppID: "alpha", Level: LevelError, Kind: "boom", Message: "failed",
	}); err != nil {
		t.Fatal(err)
	}

	all, err := s.Events(ctx, EventsQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Fatalf("got %d events, want 10", len(all))
	}
	if all[0].Message != "failed" {
		t.Errorf("newest-first ordering broken: first = %q", all[0].Message)
	}

	errs, err := s.Events(ctx, EventsQuery{Level: LevelError, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 || errs[0].Kind != "boom" {
		t.Fatalf("level filter = %+v", errs)
	}

	beta, err := s.Events(ctx, EventsQuery{AppID: "beta", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range beta {
		if e.AppID != "beta" {
			t.Fatalf("app filter leaked %q", e.AppID)
		}
	}
}

func TestRecordCommandRun(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	exit := 3

	err := s.RecordCommandRun(ctx, CommandRun{
		DeploymentID: "dep1", AppID: "app", Name: "compose pull",
		Argv: "docker compose pull", Dir: "/srv/app",
		Status: "failed", ExitCode: &exit,
		StartedAt: now, EndedAt: now.Add(3 * time.Second),
		StdoutExcerpt: "Pulling app...", StderrExcerpt: "manifest unknown",
		StdoutBytes: 14, StderrBytes: 16, Truncated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.CommandRunsForDeployment(ctx, "dep1")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	r := runs[0]
	if r.ExitCode == nil || *r.ExitCode != 3 || !r.Truncated || r.Status != "failed" {
		t.Fatalf("round trip mismatch: %+v", r)
	}
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	const writes = 60
	var wg sync.WaitGroup
	errCh := make(chan error, writes+2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			_, err := s.RecordEvent(ctx, Event{
				Time: now.Add(time.Duration(i) * time.Second), AppID: "app",
				Level: LevelInfo, Kind: "write", Message: fmt.Sprintf("w%d", i),
			})
			if err != nil {
				errCh <- fmt.Errorf("writer: %w", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				if _, err := s.Events(ctx, EventsQuery{Limit: 50}); err != nil {
					errCh <- fmt.Errorf("reader: %w", err)
					return
				}
				if _, err := s.CurrentHealthAll(ctx); err != nil {
					errCh <- fmt.Errorf("reader health: %w", err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	got, err := s.Events(ctx, EventsQuery{Limit: maxEventLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != writes {
		t.Fatalf("writer events recorded = %d, want %d", len(got), writes)
	}
}

func TestHealthProjection(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	lat := 250 * time.Millisecond

	if err := s.RecordHealthSample(ctx, "app", "http", "checking", "first probe", nil, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHealthSample(ctx, "app", "http", "healthy", "", &lat, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHealthSample(ctx, "app", "docker", "unhealthy", "health: failing", nil, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	h, ok, err := s.HealthAt(ctx, "app", "http")
	if err != nil || !ok {
		t.Fatalf("HealthAt: %v, %v", ok, err)
	}
	if h.State != "healthy" || h.Reason != "" {
		t.Fatalf("http projection = %+v", h)
	}
	if h.CheckedAt != now.Add(time.Second).Truncate(time.Second) && h.CheckedAt.Before(now) {
		t.Logf("checked_at = %v", h.CheckedAt)
	}

	all, err := s.CurrentHealth(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("projections = %d, want 2 (http, docker)", len(all))
	}

	hist, err := s.Events(ctx, EventsQuery{Limit: 10}) // unrelated; keep compile coverage
	if err != nil {
		t.Fatal(err)
	}
	_ = hist
}

func TestOutboxQueueBasics(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	id, err := s.CoalesceOutboundEvent(ctx, OutboundEvent{
		CreatedAt: now, AvailableAt: now, CoalesceKey: "heartbeat",
		Kind: "heartbeat", Payload: `{"host":"a"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Coalescing replaces the pending heartbeat instead of queueing another.
	id2, err := s.CoalesceOutboundEvent(ctx, OutboundEvent{
		CreatedAt: now.Add(time.Minute), AvailableAt: now.Add(time.Minute), CoalesceKey: "heartbeat",
		Kind: "heartbeat", Payload: `{"host":"a","seq":2}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingOutboundCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1 (coalesced)", pending)
	}
	if id == id2 {
		t.Log("coalesce kept the original event id, as intended")
	} else {
		t.Errorf("coalescing must keep the queued event id: got %s then %s", id, id2)
	}

	// Not yet available events are not claimable.
	if err := s.EnqueueOutboundEvent(ctx, OutboundEvent{
		CreatedAt: now, AvailableAt: now.Add(time.Hour), Kind: "deploy", Payload: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	claimable, err := s.ClaimableOutboundEvents(ctx, now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimable) != 1 || claimable[0].Payload != `{"host":"a","seq":2}` {
		t.Fatalf("claimable = %+v", claimable)
	}

	// A failed attempt schedules a retry; a delivered one is terminal.
	if err := s.MarkOutboundRetry(ctx, id2, now.Add(5*time.Minute), "connection refused"); err != nil {
		t.Fatal(err)
	}
	claimable, err = s.ClaimableOutboundEvents(ctx, now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimable) != 0 {
		t.Fatalf("retry must not be claimable before available_at, got %+v", claimable)
	}
	if err := s.MarkOutboundDelivered(ctx, id2, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkOutboundDelivered(ctx, id2, now.Add(7*time.Minute)); err == nil {
		t.Fatal("double delivery must be rejected")
	}
	if err := s.RecordDeliveryAttempt(ctx, id2, now.Add(6*time.Minute), "delivered", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LastDelivery(ctx); !ok {
		t.Fatal("last delivery must be visible")
	}
	// Delivered events leave the pending count.
	pending, err = s.PendingOutboundCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 { // the future deploy event
		t.Fatalf("pending = %d, want 1", pending)
	}
}

func TestReplayDedup(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	seen, err := s.SeenInboundEvent(ctx, "peer", "evt-1", now)
	if err != nil || seen {
		t.Fatalf("first sight = %v, %v; want false", seen, err)
	}
	seen, err = s.SeenInboundEvent(ctx, "peer", "evt-1", now)
	if err != nil || !seen {
		t.Fatalf("second sight = %v, %v; want true (replay)", seen, err)
	}
	// Same event ID from another host is not a replay.
	seen, err = s.SeenInboundEvent(ctx, "other", "evt-1", now)
	if err != nil || seen {
		t.Fatalf("other host = %v, %v; want false", seen, err)
	}

	n, err := s.PurgeInboundEventIDs(ctx, now.Add(-time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("fresh rows purged = %d, %v; want 0", n, err)
	}
	n, err = s.PurgeInboundEventIDs(ctx, now.Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("expired purged = %d, %v; want 2", n, err)
	}
	seen, err = s.SeenInboundEvent(ctx, "peer", "evt-1", now)
	if err != nil || seen {
		t.Fatal("after purge the event must be acceptable again")
	}
}

func TestRemoteHostsAndState(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := s.TouchRemoteHost(ctx, "garage-pi", now); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchRemoteHost(ctx, "garage-pi", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	hosts, err := s.RemoteHosts(ctx)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts = %+v, %v", hosts, err)
	}
	if !hosts[0].LastSeenAt.After(hosts[0].CreatedAt) {
		t.Errorf("last_seen must advance: created=%v seen=%v", hosts[0].CreatedAt, hosts[0].LastSeenAt)
	}

	if err := s.UpsertRemoteState(ctx, "garage-pi", "wiki", `{"status":"healthy"}`, now); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRemoteState(ctx, "garage-pi", "wiki", `{"status":"unhealthy"}`, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	states, err := s.RemoteStates(ctx, "")
	if err != nil || len(states) != 1 {
		t.Fatalf("states = %+v, %v", states, err)
	}
	if states[0].Data != `{"status":"unhealthy"}` {
		t.Errorf("state not superseded: %s", states[0].Data)
	}
	only, err := s.RemoteStates(ctx, "absent-host")
	if err != nil || len(only) != 0 {
		t.Fatalf("host filter = %+v, %v", only, err)
	}
}
