package daemon

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/schedule"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
	"github.com/Dragonshorn-Studios/yukariko/internal/webhooks"
)

func quietWebhookLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The event sink fans deployment outcomes out to webhooks: one pending
// delivery per configured target, enriched from the deployment row.
// Enqueue failures are logged and never fail the sink.
func TestEventSinkEnqueuesWebhooksOnDeploymentOutcome(t *testing.T) {
	t.Parallel()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Second)
	depID, err := st.BeginDeployment(context.Background(), store.BeginDeploymentParams{
		AppID: "web", Cause: "scheduled", FromVersion: "aaaaaaaaaaaa", ToVersion: "bbbbbbbbbbbb", At: now,
	})
	if err != nil {
		t.Fatalf("BeginDeployment: %v", err)
	}
	if err := st.CommitDeploymentSuccess(context.Background(), "web", "digest", "sha256:deadbeef", depID, now); err != nil {
		t.Fatalf("CommitDeploymentSuccess: %v", err)
	}

	dispatcher := &webhooks.Dispatcher{
		Store: st,
		Hooks: []config.Webhook{{Name: "amadeus"}, {Name: "other"}},
		Log:   quietWebhookLogger(),
	}
	sink := &eventSink{store: st, webhooks: dispatcher}

	err = sink.RecordAppEvent(context.Background(), schedule.Event{
		AppID: "web", Time: now, Kind: schedule.EventState,
		From: schedule.StateDeploying, To: schedule.StateSucceeded, Detail: "pull; up -d --wait",
	})
	if err != nil {
		t.Fatalf("RecordAppEvent failed: %v (enqueue failures must be logged, not returned)", err)
	}
	rows, err := st.ClaimableWebhookDeliveries(context.Background(), now.Add(time.Minute), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("claimable = %d, %v; want one per webhook", len(rows), err)
	}
	for _, row := range rows {
		if row.Event != webhooks.EventDeploymentSucceeded {
			t.Errorf("event = %q", row.Event)
		}
	}

	// Non-deployment events enqueue nothing.
	if err := sink.RecordAppEvent(context.Background(), schedule.Event{
		AppID: "web", Time: now, Kind: schedule.EventState,
		From: schedule.StateChecking, To: schedule.StateIdle, Detail: "up-to-date",
	}); err != nil {
		t.Fatalf("RecordAppEvent(check) failed: %v", err)
	}
	rows, _ = st.ClaimableWebhookDeliveries(context.Background(), now.Add(time.Minute), 10)
	if len(rows) != 2 {
		t.Errorf("claimable after non-deploy event = %d, want 2 (unchanged)", len(rows))
	}
}

func TestAssembleBuildsWebhookDispatcherWhenConfigured(t *testing.T) {
	t.Parallel()
	withHooks := &config.Config{SchemaVersion: 1, Webhooks: []config.Webhook{
		{Name: "amadeus", URL: "https://amadeus.example.com/hook"},
	}}
	asm, err := Assemble(context.Background(), Options{Config: withHooks, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	defer asm.Store.Close()
	if asm.Webhooks == nil {
		t.Fatal("Webhooks = nil, want the dispatcher wired when targets are configured")
	}
	if len(asm.Webhooks.Hooks) != 1 || asm.Webhooks.Hooks[0].Name != "amadeus" {
		t.Errorf("hooks = %+v; want amadeus carried through", asm.Webhooks.Hooks)
	}
	if asm.Webhooks.Sender == nil {
		t.Error("dispatcher sender must be wired")
	}

	plain := &config.Config{SchemaVersion: 1}
	asm, err = Assemble(context.Background(), Options{Config: plain, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Assemble without webhooks: %v", err)
	}
	defer asm.Store.Close()
	if asm.Webhooks != nil {
		t.Error("Webhooks != nil, want nil when no targets are configured")
	}
}
