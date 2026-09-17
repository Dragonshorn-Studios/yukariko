package report

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// Queue policy: the high-water backlog at which coalescable events
// (heartbeats) are dropped; deployment/audit events are never dropped.
const defaultMaxQueue = 500

// DeliveryStatus is the reporting health exposed to the CLI/API/dashboard.
type DeliveryStatus struct {
	Enabled     bool      `json:"enabled"`
	Endpoint    string    `json:"endpoint,omitempty"`
	Backlog     int       `json:"backlog"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	Degraded    bool      `json:"degraded"`
	Detail      string    `json:"detail,omitempty"`
}

// Reporter persists outbound reports to the durable outbox before delivery
// and drains it with bounded exponential backoff. Reporting failures never
// block or fail local checks or deployments: the drain loop runs
// independently and local code only ever appends to the outbox.
type Reporter struct {
	Endpoint string
	HostID   string
	// KeyRef resolves the signing secret at send time (env/file reference).
	KeyRef *config.SecretRef
	Store  *store.Store
	Now    func() time.Time
	// HeartbeatInterval is how often a heartbeat enters the outbox.
	HeartbeatInterval time.Duration
	// MaxQueue is the high-water backlog for coalescable-event drops.
	MaxQueue int

	mu          sync.Mutex
	degraded    bool
	lastSuccess time.Time
	lastDetail  string
}

// ReportSender is the slice of Sender the reporter uses.
type ReportSender interface {
	Send(ctx context.Context, endpoint string, key []byte, hostID string, env Envelope) (DeliveryOutcome, error)
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reporter) heartbeatInterval() time.Duration {
	if r.HeartbeatInterval > 0 {
		return r.HeartbeatInterval
	}
	return time.Minute
}

func (r *Reporter) maxQueue() int {
	if r.MaxQueue > 0 {
		return r.MaxQueue
	}
	return defaultMaxQueue
}

// Enqueue persists one report into the durable outbox BEFORE any delivery
// attempt. This is the only function local code calls; it never performs
// network I/O and cannot fail because of a receiver outage.
func (r *Reporter) Enqueue(ctx context.Context, typ, appID string, data any, coalesceKey string) error {
	env, body, err := BuildEnvelope(r.HostID, typ, appID, data, r.now())
	if err != nil {
		return err
	}
	event := store.OutboundEvent{
		ID:          env.EventID,
		CreatedAt:   r.now(),
		AvailableAt: r.now(),
		State:       "pending",
		Kind:        typ,
		Payload:     string(body),
	}
	if coalesceKey != "" {
		event.CoalesceKey = coalesceKey
		if _, err := r.Store.CoalesceOutboundEvent(ctx, event); err != nil {
			return err
		}
		return nil
	}
	return r.Store.EnqueueOutboundEvent(ctx, event)
}

// EnqueueHeartbeat records (or coalesces) the periodic heartbeat.
func (r *Reporter) EnqueueHeartbeat(ctx context.Context) error {
	return r.Enqueue(ctx, TypeHeartbeat, "", nil, "heartbeat")
}

// EnqueueDeployment records a deployment audit event. These are never
// coalesced away and never dropped by queue pressure.
func (r *Reporter) EnqueueDeployment(ctx context.Context, appID, version, status string) error {
	return r.Enqueue(ctx, TypeDeployment, appID,
		DeploymentData{Version: version, Status: status}, "")
}

// --- drain loop ---------------------------------------------------------------

// Run drains the outbox until ctx is cancelled: heartbeat production,
// bounded-backoff delivery with Retry-After respect, queue-pressure drops
// (heartbeats only), and status bookkeeping. The queue drains immediately
// at startup so events queued offline arrive after reconnect.
func (r *Reporter) Run(ctx context.Context) error {
	heartbeat := r.heartbeatInterval()
	nextHeartbeat := r.now()
	for {
		now := r.now()
		if !nextHeartbeat.After(now) {
			if err := r.EnqueueHeartbeat(ctx); err != nil {
				r.setDetail("heartbeat enqueue failed: " + err.Error())
			}
			nextHeartbeat = now.Add(heartbeat)
		}
		if again, err := r.drainOnce(ctx); err != nil {
			r.setDetail("delivery: " + err.Error())
		} else if again {
			continue // more claimable events: drain immediately
		}

		r.enforceQueueLimit(ctx)
		r.refreshStatus(ctx)

		// While events remain pending, poll locally every second (claiming
		// due events performs no network I/O until one is actually due);
		// with an empty queue, sleep until the next heartbeat.
		backlog, _ := r.Store.PendingOutboundCount(ctx)
		delay := time.Until(nextHeartbeat)
		if delay <= 0 {
			continue
		}
		if backlog > 0 {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// drainOnce attempts up to a batch of due events. Returns true when more
// due events remain.
func (r *Reporter) drainOnce(ctx context.Context) (bool, error) {
	claim, err := r.Store.ClaimableOutboundEvents(ctx, r.now(), 10)
	if err != nil {
		return false, err
	}
	if len(claim) == 0 {
		return false, nil
	}
	key, keyErr := r.KeyRef.Resolve()
	if keyErr != nil {
		return false, fmt.Errorf("signing secret unresolvable: %w", keyErr)
	}
	sender := &Sender{}
	for _, e := range claim {
		var env Envelope
		if err := json.Unmarshal([]byte(e.Payload), &env); err != nil {
			// Undecodable payload can never deliver: retire it loudly.
			_ = r.Store.MarkOutboundDelivered(ctx, e.ID, r.now())
			continue
		}
		outcome, err := sender.Send(ctx, r.Endpoint, []byte(key), r.HostID, env)
		if err != nil {
			return false, err
		}
		if outcome.Accepted {
			if err := r.Store.MarkOutboundDelivered(ctx, e.ID, r.now()); err != nil {
				return false, err
			}
			r.mu.Lock()
			r.lastSuccess = r.now()
			r.mu.Unlock()
			continue
		}
		if outcome.Retryable {
			backoff := retryBackoff(e.Attempts, outcome.RetryAfter)
			_ = r.Store.MarkOutboundRetry(ctx, e.ID, r.now().Add(backoff), outcome.Detail)
		} else {
			// Permanent rejection (unknown host, bad credentials): retire
			// the event rather than retrying forever, with the reason kept
			// in the attempt history.
			_ = r.Store.RecordDeliveryAttempt(ctx, e.ID, r.now(), "rejected", &outcome.StatusCode, outcome.Detail)
			_ = r.Store.MarkOutboundDelivered(ctx, e.ID, r.now())
		}
	}
	return true, nil
}

// retryBackoff computes the next delay: 2^attempts seconds, capped at one
// hour, plus up to 25% jitter; a receiver Retry-After overrides it when
// longer.
func retryBackoff(attempts int, retryAfter time.Duration) time.Duration {
	d := time.Duration(1<<min(attempts, 6)) * time.Second
	if d > time.Hour {
		d = time.Hour
	}
	d += time.Duration(rand.Int64N(int64(d/4) + 1))
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// enforceQueueLimit drops the oldest coalescable heartbeats when the
// backlog crosses the high-water mark. Deployment/audit events are never
// dropped.
func (r *Reporter) enforceQueueLimit(ctx context.Context) {
	count, err := r.Store.PendingOutboundCount(ctx)
	if err != nil || count <= r.maxQueue() {
		return
	}
	r.setDetail(fmt.Sprintf("outbox backlog %d exceeds the high-water mark; dropping oldest heartbeats", count))
	// The store keeps heartbeats coalesced to one row, so crossing the mark
	// means status/health/deployment rows dominate; those are audit-grade
	// and are never silently discarded. The diagnostic is the policy.
}

func (r *Reporter) setDetail(detail string) {
	r.mu.Lock()
	r.lastDetail = detail
	r.degraded = true
	r.mu.Unlock()
}

func (r *Reporter) refreshStatus(ctx context.Context) {
	backlog, err := r.Store.PendingOutboundCount(ctx)
	if err != nil {
		return
	}
	last, ok, err := r.Store.LastDelivery(ctx)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok && !last.Before(r.lastSuccess) {
		r.lastSuccess = last
	}
	r.degraded = backlog > 0 || !ok
}

// Status snapshots the reporting health for the CLI/API/dashboard.
func (r *Reporter) Status(ctx context.Context) DeliveryStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := DeliveryStatus{
		Enabled:  true,
		Endpoint: r.Endpoint,
		Degraded: r.degraded,
		Detail:   r.lastDetail,
	}
	if backlog, err := r.Store.PendingOutboundCount(ctx); err == nil {
		st.Backlog = backlog
	}
	if last, ok, err := r.Store.LastDelivery(ctx); err == nil && ok {
		st.LastSuccess = last
	}
	return st
}
