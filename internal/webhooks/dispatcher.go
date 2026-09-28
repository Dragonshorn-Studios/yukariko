package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// Delivery retry policy, mirroring the #18 outbound outbox: bounded
// exponential backoff with jitter; a receiver Retry-After wins when longer;
// past the attempt cap the delivery is abandoned with its last error.
const (
	drainBatch = 10
	idlePoll   = 30 * time.Second
)

// MaxAttempts bounds sends per delivery; after this the row is abandoned
// and the failure stays visible in last_error.
var MaxAttempts = 8

// Dispatcher enqueues deployment outcomes for every configured webhook and
// drains the durable queue from its own goroutine. It shares nothing with
// the deploy path beyond EnqueueDeployment: a webhook outage can never
// delay, fail, or roll back a deployment.
type Dispatcher struct {
	// Store persists the delivery queue.
	Store *store.Store
	// Hooks are the configured targets (name-keyed by contract).
	Hooks []config.Webhook
	// Host identifies the reporting host in payloads.
	Host string
	// Sender performs single attempts; injectable for tests.
	Sender *Sender
	// Now and Log are injectable; nil falls back to time.Now/slog.Default.
	Now func() time.Time
	Log *slog.Logger
	// Rand is the jitter source; nil falls back to math/rand.
	Rand func(n int64) int64

	kick chan struct{}
	once sync.Once
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) jitter(n int64) int64 {
	if d.Rand != nil {
		return d.Rand(n)
	}
	return rand.Int64N(n + 1)
}

// kickCh lazily creates the wake channel so a zero-value Dispatcher works
// in tests and an idle daemon never allocates a ticker path it cannot use.
func (d *Dispatcher) kickCh() chan struct{} {
	d.once.Do(func() { d.kick = make(chan struct{}, 1) })
	return d.kick
}

// hookByName resolves a queued delivery's target. Removing a webhook from
// the configuration retires its queued deliveries at drain time.
func (d *Dispatcher) hookByName(name string) (config.Webhook, bool) {
	for _, h := range d.Hooks {
		if h.Name == name {
			return h, true
		}
	}
	return config.Webhook{}, false
}

// EnqueueDeployment persists one delivery per configured webhook for a
// deployment outcome, BEFORE any network I/O, then wakes the drain loop.
// The returned error is informational only — callers log it and move on;
// it must never fail the pass.
func (d *Dispatcher) EnqueueDeployment(ctx context.Context, appID, detail, status string) error {
	event := EventDeploymentSucceeded
	depStatus := "succeeded"
	if status != "succeeded" {
		event = EventDeploymentFailed
		depStatus = "failed"
	}

	// The deployment row carries the full from/to versions; the schedule
	// event's detail is a stage summary, not a version.
	rows, err := d.Store.RecentDeployments(ctx, appID, 5)
	if err != nil {
		return fmt.Errorf("find deployment for webhook: %w", err)
	}
	var dep *store.Deployment
	for i := range rows {
		if rows[i].Status == depStatus {
			dep = &rows[i]
			break
		}
	}
	if dep == nil {
		return fmt.Errorf("no %s deployment row found for app %q", depStatus, appID)
	}

	payload := Payload{
		Version: PayloadVersion,
		Event:   event,
		Host:    d.Host,
		App:     appID,
		Deployment: Deployment{
			ID:          dep.ID,
			Cause:       dep.Cause,
			FromVersion: dep.FromVersion,
			ToVersion:   dep.ToVersion,
			Status:      dep.Status,
			StartedAt:   rfc3339(dep.StartedAt),
			EndedAt:     rfc3339(dep.EndedAt),
		},
		Detail: detail,
		Time:   rfc3339(d.now()),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("render webhook payload: %w", err)
	}
	if len(body) > maxPayloadBytes {
		return fmt.Errorf("webhook payload is %d bytes, over the %d byte cap", len(body), maxPayloadBytes)
	}

	now := d.now()
	for _, hook := range d.Hooks {
		if err := d.Store.EnqueueWebhookDelivery(ctx, store.WebhookDelivery{
			Webhook:     hook.Name,
			Event:       event,
			Payload:     string(body),
			AvailableAt: now,
			CreatedAt:   now,
		}); err != nil {
			return fmt.Errorf("enqueue webhook %q: %w", hook.Name, err)
		}
	}
	// Non-blocking wake: the drain loop also polls.
	select {
	case d.kickCh() <- struct{}{}:
	default:
	}
	if n := len(d.Hooks); n > 0 {
		d.log().Debug("enqueued webhook deliveries", "app", appID, "webhooks", n)
	}
	return nil
}

// Run drains the delivery queue until ctx is cancelled. It drains
// immediately at startup (recovering rows parked by downtime), then waits
// on kick, a slow poll, or cancellation.
func (d *Dispatcher) Run(ctx context.Context) {
	d.drainAll(ctx)
	ticker := time.NewTicker(idlePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.kickCh():
		case <-ticker.C:
		}
		d.drainAll(ctx)
	}
}

// drainAll claims and sends batches until the claimable queue is empty,
// then leaves future-dated (backing-off) rows to the next wake.
func (d *Dispatcher) drainAll(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		batch, err := d.Store.ClaimableWebhookDeliveries(ctx, d.now(), drainBatch)
		if err != nil {
			d.log().Error("claim webhook deliveries", "error", err)
			return
		}
		if len(batch) == 0 {
			return
		}
		for _, delivery := range batch {
			if ctx.Err() != nil {
				return
			}
			d.deliverOne(ctx, delivery)
		}
	}
}

// deliverOne performs exactly one attempt for one queued delivery and
// records the outcome. No failure path here can affect anything but this
// delivery row.
func (d *Dispatcher) deliverOne(ctx context.Context, delivery store.WebhookDelivery) {
	hook, ok := d.hookByName(delivery.Webhook)
	if !ok {
		d.retire(ctx, delivery, "webhook %q is no longer configured", delivery.Webhook)
		return
	}
	att, err := d.Sender.Send(ctx, hook, delivery.Event, delivery.ID, []byte(delivery.Payload))
	switch {
	case err != nil:
		d.retryOrAbandon(ctx, delivery, err.Error())
		return
	case att.Delivered:
		if mErr := d.Store.MarkWebhookDelivered(ctx, delivery.ID, d.now()); mErr != nil {
			d.log().Error("mark webhook delivered", "delivery", delivery.ID, "error", mErr)
		}
		return
	case att.Abandon:
		detail := att.Detail
		if detail == "" {
			detail = "receiver reported the delivery permanently gone"
		}
		d.retire(ctx, delivery, "%s", detail)
		return
	default:
		detail := att.Detail
		if detail == "" {
			detail = "receiver rejected the delivery"
		}
		d.retryOrAbandon(ctx, delivery, detail, att.RetryAfter)
	}
}

// retryOrAbandon applies the backoff policy: 2^min(attempts,6) seconds
// capped at an hour plus up to 25% jitter, with a receiver Retry-After
// winning when longer. Past the attempt cap the delivery is abandoned.
func (d *Dispatcher) retryOrAbandon(ctx context.Context, delivery store.WebhookDelivery, failure string, floor ...time.Duration) {
	attempts := delivery.Attempts + 1
	if attempts >= MaxAttempts {
		d.retire(ctx, delivery, "abandoned after %d attempts: %s", attempts, failure)
		return
	}
	backoff := time.Duration(1<<min(attempts, 6)) * time.Second
	if backoff > time.Hour {
		backoff = time.Hour
	}
	backoff += time.Duration(d.jitter(int64(backoff) / 4)) // ≤25% jitter
	for _, f := range floor {
		if f > backoff {
			backoff = f
		}
	}
	next := d.now().Add(backoff)
	if mErr := d.Store.MarkWebhookRetry(ctx, delivery.ID, next, failure); mErr != nil {
		d.log().Error("mark webhook retry", "delivery", delivery.ID, "error", mErr)
	}
}

// retire abandons a delivery with a rendered diagnosis. Abandoned rows are
// the dead-letter record; nothing is deleted.
func (d *Dispatcher) retire(ctx context.Context, delivery store.WebhookDelivery, format string, args ...any) {
	if mErr := d.Store.MarkWebhookAbandoned(ctx, delivery.ID, fmt.Sprintf(format, args...)); mErr != nil {
		d.log().Error("mark webhook abandoned", "delivery", delivery.ID, "error", mErr)
	}
}
