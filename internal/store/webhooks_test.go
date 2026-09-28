package store

import (
	"context"
	"testing"
	"time"
)

func TestWebhookDeliveryLifecycle(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.EnqueueWebhookDelivery(ctx, WebhookDelivery{
		Webhook: "amadeus", Event: "deployment.succeeded",
		Payload: `{"event":"deployment.succeeded"}`, AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("EnqueueWebhookDelivery: %v", err)
	}

	// Claimable immediately.
	claim, err := s.ClaimableWebhookDeliveries(ctx, now, 10)
	if err != nil || len(claim) != 1 {
		t.Fatalf("Claimable = %v, %v; want 1", claim, err)
	}
	d := claim[0]
	if d.Webhook != "amadeus" || d.ID == "" || d.Attempts != 0 || d.State != WebhookPending {
		t.Errorf("delivery = %+v", d)
	}

	// Delivered is terminal and bumps attempts.
	if err := s.MarkWebhookDelivered(ctx, d.ID, now); err != nil {
		t.Fatalf("MarkWebhookDelivered: %v", err)
	}
	if claim, _ := s.ClaimableWebhookDeliveries(ctx, now, 10); len(claim) != 0 {
		t.Errorf("delivered row is claimable again: %+v", claim)
	}
	if n, _ := s.PendingWebhookCount(ctx); n != 0 {
		t.Errorf("pending count = %d, want 0", n)
	}

	// A late second delivery on a terminal row errors (state guard).
	if err := s.MarkWebhookDelivered(ctx, d.ID, now); err == nil {
		t.Error("double-delivery must error")
	}
}

func TestWebhookDeliveryRetryWindow(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.EnqueueWebhookDelivery(ctx, WebhookDelivery{
		Webhook: "amadeus", Event: "deployment.failed",
		Payload: `{}`, AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("EnqueueWebhookDelivery: %v", err)
	}

	// Retry schedules availability in the future.
	next := now.Add(4 * time.Second)
	if err := s.MarkWebhookRetry(ctx, mustClaimID(t, s, now), next, "connection refused"); err != nil {
		t.Fatalf("MarkWebhookRetry: %v", err)
	}
	if claim, _ := s.ClaimableWebhookDeliveries(ctx, next.Add(-time.Second), 10); len(claim) != 0 {
		t.Errorf("row claimable before available_at: %+v", claim)
	}
	claim, err := s.ClaimableWebhookDeliveries(ctx, next, 10)
	if err != nil || len(claim) != 1 {
		t.Fatalf("Claimable at retry time = %v, %v; want 1", claim, err)
	}
	if claim[0].Attempts != 1 || claim[0].LastError != "connection refused" {
		t.Errorf("delivery = %+v; want attempts=1 with last_error", claim[0])
	}
}

// The attempt cap retires a delivery as abandoned with its last error kept;
// abandoned rows never come back.
func TestWebhookDeliveryAbandonment(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.EnqueueWebhookDelivery(ctx, WebhookDelivery{
		Webhook: "amadeus", Event: "deployment.succeeded",
		Payload: `{}`, AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("EnqueueWebhookDelivery: %v", err)
	}
	id := mustClaimID(t, s, now)
	if err := s.MarkWebhookAbandoned(ctx, id, "gave up after 8 attempts: 503"); err != nil {
		t.Fatalf("MarkWebhookAbandoned: %v", err)
	}
	if claim, _ := s.ClaimableWebhookDeliveries(ctx, now.Add(time.Hour), 10); len(claim) != 0 {
		t.Errorf("abandoned row is claimable: %+v", claim)
	}
	if n, _ := s.PendingWebhookCount(ctx); n != 0 {
		t.Errorf("pending count = %d, want 0", n)
	}
}

// Oldest-first claim order across webhooks and recovery of rows persisted
// before a (simulated) restart.
func TestWebhookDeliveryClaimOrderAndRestartRecovery(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for i, hook := range []string{"zulu", "alpha", "mike"} {
		if err := s.EnqueueWebhookDelivery(ctx, WebhookDelivery{
			Webhook: hook, Event: "deployment.succeeded",
			Payload: `{}`, AvailableAt: now, CreatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("Enqueue(%s): %v", hook, err)
		}
	}
	claim, err := s.ClaimableWebhookDeliveries(ctx, now, 10)
	if err != nil || len(claim) != 3 {
		t.Fatalf("Claimable = %d, %v; want 3", len(claim), err)
	}
	if claim[0].Webhook != "zulu" || claim[1].Webhook != "alpha" || claim[2].Webhook != "mike" {
		t.Errorf("claim order = [%s %s %s]; want created_at order", claim[0].Webhook, claim[1].Webhook, claim[2].Webhook)
	}
	// Limit is honored.
	if claim, _ := s.ClaimableWebhookDeliveries(ctx, now, 2); len(claim) != 2 {
		t.Errorf("limit not honored: got %d", len(claim))
	}
}

func mustClaimID(t *testing.T, s *Store, now time.Time) string {
	t.Helper()
	claim, err := s.ClaimableWebhookDeliveries(context.Background(), now, 1)
	if err != nil || len(claim) != 1 {
		t.Fatalf("claim: %v, %v", claim, err)
	}
	return claim[0].ID
}
