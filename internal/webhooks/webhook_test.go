package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedDeployment records one finished deployment and returns its ID. The
// success path goes through CommitDeploymentSuccess (the only writer of
// succeeded rows); failures through FinishDeployment.
func seedDeployment(t *testing.T, st *store.Store, appID, status string) string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	id, err := st.BeginDeployment(context.Background(), store.BeginDeploymentParams{
		AppID: appID, Cause: "scheduled", FromVersion: "aaaaaaaaaaaa", ToVersion: "bbbbbbbbbbbb", At: now,
	})
	if err != nil {
		t.Fatalf("BeginDeployment: %v", err)
	}
	if status == "succeeded" {
		if err := st.CommitDeploymentSuccess(context.Background(), appID, "digest", "sha256:deadbeef", id, now.Add(time.Minute)); err != nil {
			t.Fatalf("CommitDeploymentSuccess: %v", err)
		}
		return id
	}
	if err := st.FinishDeployment(context.Background(), id, status, "", now.Add(time.Minute)); err != nil {
		t.Fatalf("FinishDeployment: %v", err)
	}
	return id
}

// receiver records signed deliveries and answers per a script.
type receiver struct {
	mu       sync.Mutex
	bodies   []string
	events   []string
	delivery []string
	ts       []string
	sig      []string
	statuses []int // answered round-robin; last repeats
	server   *httptest.Server
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	r := &receiver{statuses: statuses}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(body))
		r.events = append(r.events, req.Header.Get(HeaderEvent))
		r.delivery = append(r.delivery, req.Header.Get(HeaderDelivery))
		r.ts = append(r.ts, req.Header.Get(HeaderTimestamp))
		r.sig = append(r.sig, req.Header.Get(HeaderSignature))
		r.mu.Unlock()
		i := len(r.bodies) - 1
		if i >= len(r.statuses) {
			i = len(r.statuses) - 1
		}
		w.WriteHeader(r.statuses[i])
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// verifyLast recomputes the HMAC over the last received delivery.
func (r *receiver) verifyLast(t *testing.T, secret string) bool {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(r.ts[len(r.ts)-1] + "." + sha256Hex([]byte(r.bodies[len(r.bodies)-1]))))
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(r.sig[len(r.sig)-1]), []byte(want))
}

func TestSenderSignsAndDelivers(t *testing.T) {
	t.Parallel()
	rec := newReceiver(t, 200)
	hook := config.Webhook{
		Name:      "amadeus",
		URL:       rec.server.URL,
		SecretRef: &config.SecretRef{File: writeSecret(t, "hook-secret")},
		Headers:   []config.WebhookHeader{{Name: "X-Tenant", Value: "ops"}},
	}
	s := &Sender{Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	body := []byte(`{"event":"deployment.succeeded"}`)

	att, err := s.Send(context.Background(), hook, EventDeploymentSucceeded, "del-1", body)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !att.Delivered {
		t.Errorf("attempt = %+v; want delivered", att)
	}
	if rec.count() != 1 {
		t.Fatalf("receiver saw %d deliveries, want 1", rec.count())
	}
	if got := rec.events[0]; got != EventDeploymentSucceeded {
		t.Errorf("event header = %q", got)
	}
	if got := rec.delivery[0]; got != "del-1" {
		t.Errorf("delivery header = %q", got)
	}
	if !rec.verifyLast(t, "hook-secret") {
		t.Error("signature did not verify against the shared secret")
	}
}

func writeSecret(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSenderClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		status   int
		extra    func(w http.ResponseWriter)
		wantDel  bool
		wantAban bool
		wantRA   time.Duration
	}{
		{name: "200 delivers", status: 200, wantDel: true},
		{name: "204 delivers", status: 204, wantDel: true},
		{name: "410 abandons", status: 410, wantAban: true},
		{name: "429 honors Retry-After", status: 429, extra: func(w http.ResponseWriter) { w.Header().Set("Retry-After", "120") }, wantRA: 120 * time.Second},
		{name: "500 retries", status: 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.extra != nil {
					c.extra(w)
				}
				w.WriteHeader(c.status)
			}))
			t.Cleanup(srv.Close)
			s := &Sender{}
			att, err := s.Send(context.Background(), config.Webhook{Name: "h", URL: srv.URL, Timeout: config.Duration(time.Second)}, "e", "d", []byte("{}"))
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if att.Delivered != c.wantDel || att.Abandon != c.wantAban || att.RetryAfter != c.wantRA {
				t.Errorf("attempt = %+v; want delivered=%v abandon=%v retryAfter=%v", att, c.wantDel, c.wantAban, c.wantRA)
			}
		})
	}

	// A transport failure is an error, not a classified attempt.
	s := &Sender{}
	if _, err := s.Send(context.Background(), config.Webhook{Name: "h", URL: "http://127.0.0.1:1/hook", Timeout: config.Duration(time.Second)}, "e", "d", []byte("{}")); err == nil {
		t.Error("Send to a dead port must error")
	}

	// An unresolvable signing secret is a send failure, never a plaintext
	// fallback.
	rec := newReceiver(t, 200)
	broken := config.Webhook{Name: "h", URL: rec.server.URL, SecretRef: &config.SecretRef{Env: "DEFINITELY_UNSET_VAR_1234"}, Timeout: config.Duration(time.Second)}
	if _, err := s.Send(context.Background(), broken, "e", "d", []byte("{}")); err == nil {
		t.Error("unresolvable secret must error")
	}
	if rec.count() != 0 {
		t.Error("no request may leave with an unresolved secret")
	}
}

// Enqueue persists one row per webhook before any send: with the drain loop
// never started and a receiver that would fail, the rows sit pending.
func TestEnqueuePersistsBeforeSend(t *testing.T) {
	t.Parallel()
	st := testStore(t)
	hooks := []config.Webhook{{Name: "a"}, {Name: "b"}}
	d := &Dispatcher{Store: st, Hooks: hooks, Host: "local", Sender: &Sender{}, Log: quietLogger(),
		Now: func() time.Time { return time.Now().UTC().Truncate(time.Second) }}

	seedDeployment(t, st, "web", "succeeded")
	if err := d.EnqueueDeployment(context.Background(), "web", "stages ok", "succeeded"); err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	rows, err := st.ClaimableWebhookDeliveries(context.Background(), time.Now().Add(time.Minute), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("claimable = %d, %v; want 2", len(rows), err)
	}
	for _, row := range rows {
		if row.State != store.WebhookPending || row.Attempts != 0 {
			t.Errorf("row = %+v; want fresh pending", row)
		}
		var p Payload
		if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
			t.Fatalf("payload decode: %v", err)
		}
		if p.Version != PayloadVersion || p.Event != EventDeploymentSucceeded || p.App != "web" {
			t.Errorf("payload = %+v", p)
		}
		if p.Deployment.ToVersion != "bbbbbbbbbbbb" || p.Deployment.ID == "" {
			t.Errorf("deployment block = %+v; want full versions and id", p.Deployment)
		}
	}
	// Named webhooks each got one row.
	seen := map[string]bool{rows[0].Webhook: true, rows[1].Webhook: true}
	if !seen["a"] || !seen["b"] {
		t.Errorf("webhooks enqueued = %v; want a and b", seen)
	}
}

func TestEnqueueFailsWithoutDeploymentRow(t *testing.T) {
	t.Parallel()
	st := testStore(t)
	d := &Dispatcher{Store: st, Hooks: []config.Webhook{{Name: "a"}}, Log: quietLogger()}
	if err := d.EnqueueDeployment(context.Background(), "ghost", "", "succeeded"); err == nil {
		t.Error("enqueue without a deployment row must error (and the caller logs it)")
	}
}

// The drain loop delivers, retries on failure with backoff, and abandons at
// the attempt cap.
func TestDispatcherDrainRetryAndAbandon(t *testing.T) {
	t.Parallel()
	st := testStore(t)
	rec := newReceiver(t, 500, 500, 500, 500, 500, 500, 500, 500) // always fail
	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	d := &Dispatcher{
		Store:  st,
		Hooks:  []config.Webhook{{Name: "amadeus", URL: rec.server.URL, Timeout: config.Duration(time.Second)}},
		Sender: &Sender{Now: func() time.Time { return clock }},
		Now:    func() time.Time { return clock },
		Rand:   func(int64) int64 { return 0 },
		Log:    quietLogger(),
	}

	seedDeployment(t, st, "web", "failed")
	if err := d.EnqueueDeployment(context.Background(), "web", "boom", "failed"); err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}

	// Drain until abandoned: each drain sends one attempt per claimable row.
	for i := 0; i < MaxAttempts+1 && rec.count() < MaxAttempts; i++ {
		clock = clock.Add(time.Hour) // blow past every backoff window
		d.drainAll(context.Background())
	}
	if got := rec.count(); got != MaxAttempts {
		t.Fatalf("receiver saw %d attempts, want %d (the cap)", got, MaxAttempts)
	}
	rows, _ := st.ClaimableWebhookDeliveries(context.Background(), clock.Add(time.Hour), 10)
	if len(rows) != 0 {
		t.Fatalf("abandoned delivery still claimable: %+v", rows)
	}
	// The failure is visible in the row (query via a fresh claim would not
	// show terminal rows; inspect through the public pending count = 0 and
	// the receiver-side evidence).
	if n, _ := st.PendingWebhookCount(context.Background()); n != 0 {
		t.Errorf("pending count = %d, want 0 after abandonment", n)
	}
	if got := rec.events[0]; got != EventDeploymentFailed {
		t.Errorf("event = %q, want %q", got, EventDeploymentFailed)
	}
}

// A successful second attempt delivers: 500 then 200.
func TestDispatcherRetriesThenDelivers(t *testing.T) {
	t.Parallel()
	st := testStore(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	d := &Dispatcher{
		Store:  st,
		Hooks:  []config.Webhook{{Name: "amadeus", URL: srv.URL, Timeout: config.Duration(time.Second)}},
		Sender: &Sender{Now: func() time.Time { return clock }},
		Now:    func() time.Time { return clock },
		Rand:   func(int64) int64 { return 0 },
		Log:    quietLogger(),
	}
	seedDeployment(t, st, "web", "succeeded")
	if err := d.EnqueueDeployment(context.Background(), "web", "ok", "succeeded"); err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}

	d.drainAll(context.Background())
	if n, _ := st.PendingWebhookCount(context.Background()); n != 1 {
		t.Fatalf("pending after first drain = %d, want 1 (backing off)", n)
	}
	clock = clock.Add(time.Hour)
	d.drainAll(context.Background())
	if n, _ := st.PendingWebhookCount(context.Background()); n != 0 {
		t.Errorf("pending after second drain = %d, want 0 (delivered)", n)
	}
	if attempts.Load() != 2 {
		t.Errorf("receiver saw %d attempts, want 2", attempts.Load())
	}
}

// A queued delivery whose webhook was removed from the configuration is
// retired, not retried forever.
func TestDispatcherRetiresRemovedHook(t *testing.T) {
	t.Parallel()
	st := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	d := &Dispatcher{Store: st, Hooks: nil, Sender: &Sender{}, Now: func() time.Time { return now }, Log: quietLogger()}

	if err := st.EnqueueWebhookDelivery(context.Background(), store.WebhookDelivery{
		Webhook: "gone", Event: EventDeploymentSucceeded, Payload: "{}", AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	d.drainAll(context.Background())
	if n, _ := st.PendingWebhookCount(context.Background()); n != 0 {
		t.Errorf("pending = %d, want 0 (retired)", n)
	}
}

// The backoff floor from a receiver Retry-After is respected: a 120s
// Retry-After beats the 2s exponential step.
func TestDispatcherHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	st := testStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	}))
	t.Cleanup(srv.Close)
	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	d := &Dispatcher{
		Store:  st,
		Hooks:  []config.Webhook{{Name: "amadeus", URL: srv.URL, Timeout: config.Duration(time.Second)}},
		Sender: &Sender{Now: func() time.Time { return clock }},
		Now:    func() time.Time { return clock },
		Rand:   func(int64) int64 { return 0 },
		Log:    quietLogger(),
	}
	if err := st.EnqueueWebhookDelivery(context.Background(), store.WebhookDelivery{
		Webhook: "amadeus", Event: EventDeploymentSucceeded, Payload: "{}", AvailableAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	d.drainAll(context.Background())

	// One second later the delivery must still be backing off.
	rows, err := st.ClaimableWebhookDeliveries(context.Background(), clock.Add(time.Second), 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("delivery claimable 1s after a 120s Retry-After: %v, %v", rows, err)
	}
	rows, err = st.ClaimableWebhookDeliveries(context.Background(), clock.Add(120*time.Second), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("delivery not claimable after the Retry-After window: %v, %v", rows, err)
	}
}
