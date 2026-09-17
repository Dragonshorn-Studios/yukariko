package store

import (
	"context"
	"testing"
	"time"
)

func TestRetentionBoundaries(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	policy := RetentionPolicy{EventsDays: 30, HealthDays: 14, DeploymentsDays: 365}

	// Events: one strictly older than the window, one inside by a day,
	// one fresh. Only the strictly older one is removed.
	mustEvent := func(when time.Time, msg string) {
		t.Helper()
		if _, err := s.RecordEvent(ctx, Event{Time: when, Level: LevelInfo, Kind: "k", Message: msg}); err != nil {
			t.Fatal(err)
		}
	}
	mustEvent(now.AddDate(0, 0, -31), "old")
	mustEvent(now.AddDate(0, 0, -29), "edge-inside")
	mustEvent(now, "fresh")

	// Health samples likewise.
	if err := s.RecordHealthSample(ctx, "app", "http", "healthy", "", nil, now.AddDate(0, 0, -15)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHealthSample(ctx, "app", "http", "healthy", "", nil, now.AddDate(0, 0, -13)); err != nil {
		t.Fatal(err)
	}

	// Deployments: an old terminal one goes, an old running one stays.
	oldID, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "app", Cause: "scheduled", At: now.AddDate(-2, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CommitDeploymentSuccess(ctx, "app", KindGitSHA, "oldsha", oldID, now.AddDate(-2, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "app", Cause: "scheduled", At: now.AddDate(-2, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	// A terminal deployment inside the window stays.
	recentID, err := s.BeginDeployment(ctx, BeginDeploymentParams{AppID: "app", Cause: "scheduled", At: now.AddDate(0, 0, -10)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeployment(ctx, recentID, StatusFailed, "boom", now.AddDate(0, 0, -10)); err != nil {
		t.Fatal(err)
	}

	// Outbox: a pending event far beyond any retention window must survive;
	// delivered events are also not retention-managed in this issue.
	pendingID, err := s.CoalesceOutboundEvent(ctx, OutboundEvent{
		CreatedAt: now.AddDate(-1, 0, 0), AvailableAt: now.AddDate(-1, 0, 0),
		CoalesceKey: "heartbeat", Kind: "heartbeat", Payload: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Cleanup(ctx, policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 {
		t.Errorf("events removed = %d, want 1", res.Events)
	}
	if res.Health != 1 {
		t.Errorf("health samples removed = %d, want 1", res.Health)
	}
	if res.Deployments != 1 {
		t.Errorf("deployments removed = %d, want 1 (old terminal only)", res.Deployments)
	}

	events, err := s.Events(ctx, EventsQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	msgs := map[string]bool{}
	for _, e := range events {
		msgs[e.Message] = true
	}
	if msgs["old"] || !msgs["edge-inside"] || !msgs["fresh"] {
		t.Errorf("event boundary violated: %+v", msgs)
	}

	deps, err := s.RecentDeployments(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]bool{}
	for _, d := range deps {
		statuses[d.Status] = true
	}
	if !statuses["running"] {
		t.Error("running deployment must survive retention")
	}
	if !statuses["failed"] {
		t.Error("recent failed deployment must survive retention")
	}

	pending, err := s.PendingOutboundCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("outbox pending = %d, want 1; retention must never delete queue data", pending)
	}
	claimable, err := s.ClaimableOutboundEvents(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimable) != 1 || claimable[0].ID != pendingID {
		t.Errorf("pending event not claimable after cleanup: %+v", claimable)
	}

	// Replay markers are outside retention entirely.
	if _, err := s.SeenInboundEvent(ctx, "peer", "ancient", now.AddDate(-1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(ctx, policy, now); err != nil {
		t.Fatal(err)
	}
	seen, err := s.SeenInboundEvent(ctx, "peer", "ancient", now.AddDate(-1, 0, 0))
	if err != nil || !seen {
		t.Fatalf("replay marker must survive retention (seen=%v err=%v)", seen, err)
	}
}

func TestCleanupIdempotent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	policy := RetentionPolicy{EventsDays: 7, HealthDays: 7, DeploymentsDays: 7}

	for i := 0; i < 5; i++ {
		if _, err := s.RecordEvent(ctx, Event{Time: now.AddDate(0, 0, -30), Level: LevelInfo, Kind: "k", Message: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.Cleanup(ctx, policy, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Cleanup(ctx, policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Events != 5 || second.Events != 0 {
		t.Errorf("cleanup not idempotent: first=%d second=%d", first.Events, second.Events)
	}
}
