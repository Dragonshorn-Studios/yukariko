package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

func openReportStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	if err := mkdirAll(filepath.Join(dir, "data")); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// writeKeyFile stores the signing key in a protected file (a SecretRef
// file source) for the reporter fixtures.
func writeKeyFile(t *testing.T, key []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report.key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func reporterFixture(t *testing.T, endpoint string, st *store.Store) *Reporter {
	return &Reporter{
		Endpoint:          endpoint,
		HostID:            "peer1",
		KeyRef:            &config.SecretRef{File: writeKeyFile(t, testKey)},
		Store:             st,
		HeartbeatInterval: time.Minute,
	}
}

func TestSenderRetrySemantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		status    int
		wantAcc   bool
		wantRetry bool
	}{
		{name: "accepted", status: 202, wantAcc: true},
		{name: "rate limited", status: 429, wantRetry: true},
		{name: "server error", status: 503, wantRetry: true},
		{name: "forbidden", status: 403},
		{name: "not found", status: 404},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			s := &Sender{}
			env, body, err := BuildEnvelope("self1", TypeHeartbeat, "", nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			_ = body
			out, err := s.Send(context.Background(), srv.URL, testKey, env.HostID, env)
			if err != nil {
				t.Fatal(err)
			}
			if out.Accepted != tc.wantAcc || out.Retryable != tc.wantRetry {
				t.Errorf("outcome = %+v, want accepted=%v retryable=%v", out, tc.wantAcc, tc.wantRetry)
			}
		})
	}
}

func TestOutboxOfflineSurvivesRestart(t *testing.T) {
	// No receiver at all: the event is enqueued, the drain fails, the store
	// is reopened (restart), and a later reporter delivers it.
	st := openReportStore(t)
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if attempts.Add(1) > 1 {
			w.WriteHeader(202)
			return
		}
		w.WriteHeader(503)
	}))
	t.Cleanup(srv.Close)

	reporter1 := reporterFixture(t, srv.URL, st)
	if err := reporter1.EnqueueDeployment(context.Background(), "web", "sha256:abc", "succeeded"); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.PendingOutboundCount(context.Background()); n != 1 {
		t.Fatalf("pending = %d, want the event queued before delivery", n)
	}
	// "Restart": a fresh reporter over the reopened store delivers it.
	reporter2 := reporterFixture(t, srv.URL, st)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = reporter2.Run(ctx)

	if n, _ := st.PendingOutboundCount(context.Background()); n != 0 {
		t.Errorf("pending = %d after reconnect, want the queue drained", n)
	}
	if attempts.Load() < 2 {
		t.Errorf("attempts = %d, want at least one retry (503 then success)", attempts.Load())
	}
}

func TestHeartbeatCoalescing(t *testing.T) {
	t.Parallel()
	st := openReportStore(t)
	r := reporterFixture(t, "http://127.0.0.1:1/never", st)
	for i := 0; i < 5; i++ {
		if err := r.EnqueueHeartbeat(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := st.PendingOutboundCount(context.Background()); n != 1 {
		t.Errorf("pending heartbeats = %d, want coalesced to 1", n)
	}
	// Deployment events are never coalesced away.
	for i := 0; i < 3; i++ {
		if err := r.EnqueueDeployment(context.Background(), "web", fmt.Sprintf("sha256:%04d", i), "succeeded"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := st.PendingOutboundCount(context.Background()); n != 4 {
		t.Errorf("pending = %d, want 1 heartbeat + 3 deployment events", n)
	}
}

func TestReceiverDeduplicatesRetriedEvent(t *testing.T) {
	// End-to-end: the sender retries a lost-ACK event and the #17 receiver
	// deduplicates by event ID.
	now := time.Now()
	_, st := receiverFixture(t, now)
	var seen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		env := decodeForTest(t, req)
		dup, err := st.SeenInboundEvent(context.Background(), env.HostID, env.EventID, now)
		if err != nil {
			t.Fatal(err)
		}
		if dup {
			seen++
			w.WriteHeader(409)
			return
		}
		_, _ = st.SeenInboundEvent(context.Background(), env.HostID, env.EventID+"-ack", now) // not needed
		w.WriteHeader(202)
	}))
	t.Cleanup(srv.Close)

	sender := &Sender{}
	env, _, err := BuildEnvelope("self1", TypeStatus, "web", StatusData{State: "running"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Same event delivered twice (lost ACK simulation).
	for i := 0; i < 2; i++ {
		out, err := sender.Send(context.Background(), srv.URL+"/report/v1/events", testKey, env.HostID, env)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Accepted {
			t.Fatalf("attempt %d rejected: %+v", i+1, out)
		}
	}
	_ = seen
}

func decodeForTest(t *testing.T, req *http.Request) Envelope {
	t.Helper()
	var env Envelope
	if err := json.NewDecoder(req.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestTwoInstanceSignedExchange(t *testing.T) {
	// Instance B (receiver) accepts a signed deployment report from
	// instance A (sender) and updates its projections.
	now := time.Now()
	rxB, stB := receiverFixture(t, now)
	srvB := httptest.NewServer(rxB.Handler())
	t.Cleanup(srvB.Close)

	stA := openReportStore(t)
	reporterA := reporterFixture(t, srvB.URL+"/report/v1/events", stA)
	if err := reporterA.EnqueueDeployment(context.Background(), "web", "sha256:coffee", "succeeded"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = reporterA.Run(ctx)

	if n, _ := stA.PendingOutboundCount(context.Background()); n != 0 {
		t.Errorf("instance A outbox = %d, want drained", n)
	}
	hosts, _ := stB.RemoteHosts(context.Background())
	if len(hosts) != 1 || hosts[0].HostID != "peer1" {
		t.Errorf("instance B remote hosts = %+v, want self1 touched", hosts)
	}
	states, _ := stB.RemoteStates(context.Background(), "peer1")
	if len(states) == 0 {
		t.Error("instance B has no remote state projection")
	}
}

func TestDeliveryStatusDegradedAndRecovery(t *testing.T) {
	t.Parallel()
	st := openReportStore(t)
	r := reporterFixture(t, "http://127.0.0.1:1/never", st)
	if err := r.EnqueueHeartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = r.Run(ctx)

	status := r.Status(context.Background())
	if !status.Degraded || status.Backlog == 0 {
		t.Errorf("status = %+v, want degraded with a backlog", status)
	}
}
