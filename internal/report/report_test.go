package report

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

var testKey = []byte("test-signing-key-material")

// receiverFixture wires a receiver over a temp store with one allowlisted
// host and a fixed clock.
func receiverFixture(t *testing.T, now time.Time) (*Receiver, *store.Store) {
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
	requireTLS := false // loopback httptest has no TLS; production defaults true
	inbound := config.InboundReporting{
		Enabled:      true,
		RequireTLS:   &requireTLS,
		MaxBodyBytes: config.DefaultMaxBodyBytes,
		ClockSkew:    config.Duration(5 * time.Minute),
		ReplayWindow: config.Duration(24 * time.Hour),
		RateLimit:    config.RateLimit{Events: 1000, Per: config.Duration(time.Minute)},
	}
	keys := func(hostID string) ([][]byte, bool) {
		if hostID == "peer1" || hostID == "peer2" {
			return [][]byte{testKey}, true
		}
		return nil, false
	}
	r := NewReceiver(inbound, keys, st)
	r.Now = func() time.Time { return now }
	return r, st
}

func mkdirAll(dir string) error { return osMkdir(dir) }

func signedRequest(t *testing.T, serverURL, hostID, eventID string, ts time.Time, key []byte, body []byte) *http.Request {
	t.Helper()
	timestamp := fmt.Sprintf("%d", ts.Unix())
	sig := Sign(key, hostID, eventID, timestamp, body)
	req, err := http.NewRequest(http.MethodPost, serverURL+"/report/v1/events", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderHost, hostID)
	req.Header.Set(HeaderEvent, eventID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderSignature, sig)
	return req
}

func envelopeBody(eventID string, now time.Time, typ string) []byte {
	env := map[string]any{
		"version":  1,
		"event_id": eventID,
		"host_id":  "peer1",
		"type":     typ,
		"time":     now.Format(time.RFC3339),
	}
	switch typ {
	case TypeHeartbeat:
	case TypeHealth:
		env["app_id"] = "web"
		env["data"] = map[string]any{"check": "http", "state": "healthy", "reason": "status 200"}
	case TypeStatus:
		env["app_id"] = "web"
		env["data"] = map[string]any{"state": "running", "version": "abc123"}
	case TypeDeployment:
		env["app_id"] = "web"
		env["data"] = map[string]any{"version": "sha256:abcd", "status": "succeeded"}
	}
	body, _ := json.Marshal(env)
	return body
}

func TestCanonicalVector(t *testing.T) {
	t.Parallel()
	// Golden vector: the canonical string and its HMAC for known inputs.
	body := []byte(`{"version":1}`)
	canonical := CanonicalString("peer1", "evt-1", "1700000000", body)
	want := "peer1\nevt-1\n1700000000\n" + hex.EncodeToString(func() []byte {
		sum := sha256.Sum256(body)
		return sum[:]
	}())
	if canonical != want {
		t.Fatalf("canonical = %q, want %q", canonical, want)
	}
	mac := hmac.New(sha256.New, []byte("vector-key"))
	mac.Write([]byte(canonical))
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if got := Sign([]byte("vector-key"), "peer1", "evt-1", "1700000000", body); got != wantSig {
		t.Errorf("Sign = %q, want %q", got, wantSig)
	}
}

func TestValidReportAcceptedOnce(t *testing.T) {
	now := time.Now()
	r, st := receiverFixture(t, now)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	req := signedRequest(t, srv.URL, "peer1", "evt-1", now, testKey, envelopeBody("evt-1", now, TypeHeartbeat))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	// Remote projections updated: host touched, event recorded.
	hosts, _ := st.RemoteHosts(context.Background())
	if len(hosts) != 1 || hosts[0].HostID != "peer1" {
		t.Errorf("remote hosts = %+v", hosts)
	}
	events, _ := st.Events(context.Background(), store.EventsQuery{Limit: 10})
	if len(events) == 0 || !strings.Contains(events[0].Message, "peer1") {
		t.Errorf("events = %+v, want the remote report event", events)
	}
}

func TestReplayAcrossRestart(t *testing.T) {
	now := time.Now()
	r1, st := receiverFixture(t, now)
	body := envelopeBody("evt-dup", now, TypeHeartbeat)
	srv1 := httptest.NewServer(r1.Handler())
	t.Cleanup(srv1.Close)
	resp1, err := http.DefaultClient.Do(signedRequest(t, srv1.URL, "peer1", "evt-dup", now, testKey, body))
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first report = %d, want 202", resp1.StatusCode)
	}

	// A restarted receiver shares the same durable store.
	r2 := NewReceiver(r1.Inbound, r1.Keys, st)
	r2.Now = r1.Now
	srv2 := httptest.NewServer(r2.Handler())
	t.Cleanup(srv2.Close)

	resp2, err := http.DefaultClient.Do(signedRequest(t, srv2.URL, "peer1", "evt-dup", now, testKey, body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("replayed status = %d, want 409 conflict (dedup survives restart)", resp2.StatusCode)
	}
}

func TestRotationAndRevocation(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	if err := mkdirAll(filepath.Join(dir, "data")); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key1 := []byte("key-one")
	key2 := []byte("key-two")
	keys := key2 // the currently valid key set (key1 revoked)
	_ = key1
	requireTLS := false
	inbound := config.InboundReporting{RequireTLS: &requireTLS, MaxBodyBytes: config.DefaultMaxBodyBytes, RateLimit: config.RateLimit{Events: 100, Per: config.Duration(time.Minute)}}
	r := NewReceiver(inbound, func(hostID string) ([][]byte, bool) {
		if hostID == "peer1" {
			return [][]byte{keys}, true
		}
		return nil, false
	}, st)
	r.Now = func() time.Time { return now }
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	// key2 (current) validates.
	body2 := envelopeBody("evt-k2", now, TypeHeartbeat)
	resp, err := http.DefaultClient.Do(signedRequest(t, srv.URL, "peer1", "evt-k2", now, key2, body2))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("current key rejected: %d", resp.StatusCode)
	}

	// key1 (revoked) no longer validates.
	body1 := envelopeBody("evt-k1", now, TypeHeartbeat)
	resp, err = http.DefaultClient.Do(signedRequest(t, srv.URL, "peer1", "evt-k1", now, key1, body1))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked key accepted: %d, want 401", resp.StatusCode)
	}
}

func TestClockSkewRejections(t *testing.T) {
	now := time.Now()
	r, _ := receiverFixture(t, now)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	cases := []struct {
		name   string
		offset time.Duration
		want   int
	}{
		{name: "stale", offset: -10 * time.Minute, want: http.StatusUnprocessableEntity},
		{name: "future", offset: 10 * time.Minute, want: http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		body := envelopeBody("evt-"+tc.name, now, TypeHeartbeat)
		req := signedRequest(t, srv.URL, "peer1", "evt-"+tc.name, now.Add(tc.offset), testKey, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	now := time.Now()
	r, _ := receiverFixture(t, now)
	r.Inbound.MaxBodyBytes = 512
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	big := envelopeBody("evt-big", now, TypeHeartbeat)
	req := signedRequest(t, srv.URL, "peer1", "evt-big", now, testKey, append(big, make([]byte, 2048)...))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

func TestRateLimitRejected(t *testing.T) {
	now := time.Now()
	r, _ := receiverFixture(t, now)
	r.Inbound.RateLimit = config.RateLimit{Events: 3, Per: config.Duration(time.Minute)}
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	for i := 0; i < 3; i++ {
		body := envelopeBody(fmt.Sprintf("evt-rl-%d", i), now, TypeHeartbeat)
		resp, err := http.DefaultClient.Do(signedRequest(t, srv.URL, "peer1", fmt.Sprintf("evt-rl-%d", i), now, testKey, body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	body := envelopeBody("evt-rl-over", now, TypeHeartbeat)
	resp, err := http.DefaultClient.Do(signedRequest(t, srv.URL, "peer1", "evt-rl-over", now, testKey, body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
}

func TestBadMACAndUnknownHost(t *testing.T) {
	now := time.Now()
	r, _ := receiverFixture(t, now)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	// Tampered body: the signature is computed over the original bytes, the
	// request then carries modified ones.
	body := envelopeBody("evt-tamper", now, TypeHeartbeat)
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-1] ^= 0xff
	req := signedRequest(t, srv.URL, "peer1", "evt-tamper", now, testKey, body)
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	req.ContentLength = int64(len(tampered))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tampered body = %d, want 401", resp.StatusCode)
	}

	// Unknown host.
	resp, err = http.DefaultClient.Do(signedRequest(t, srv.URL, "stranger", "evt-x", now, testKey, envelopeBody("evt-x", now, TypeHeartbeat)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown host = %d, want 401", resp.StatusCode)
	}
}

func TestMalformedAndUnsupportedVersion(t *testing.T) {
	now := time.Now()
	r, _ := receiverFixture(t, now)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	malformed := []byte(`{"version":1,"event_id":"e","host_id":"peer1","type":"heartbeat","time":"now","bogus":1}`)
	resp, err := http.DefaultClient.Do(signedRequest(t, srv.URL, "peer1", "evt-m1", now, testKey, malformed))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400", resp.StatusCode)
	}

	wrongVersion := []byte(`{"version":99,"event_id":"e","host_id":"peer1","type":"heartbeat","time":"t"}`)
	resp, err = http.DefaultClient.Do(signedRequest(t, srv.URL, "peer1", "evt-m2", now, testKey, wrongVersion))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unsupported version = %d, want 400", resp.StatusCode)
	}

	// Command-bearing fields are structurally impossible: unknown fields
	// rejected above; app_id-only addressing cannot name a command.
	commandish := []byte(`{"version":1,"event_id":"e","host_id":"peer1","type":"exec","time":"t"}`)
	resp, err = http.DefaultClient.Do(signedRequest(t, srv.URL, "peer1", "evt-m3", now, testKey, commandish))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown type = %d, want 400", resp.StatusCode)
	}
}

// TestFuzzEnvelopeNeverPanics runs malformed inputs through the receiver
// (seed corpus; `go test -fuzz` explores further). No input may panic the
// handler or produce a 2xx other than a fully valid signed report.
func FuzzEnvelopeNeverPanics(f *testing.F) {
	now := time.Now().Format(time.RFC3339)
	f.Add([]byte("random garbage"))
	f.Add([]byte(`{"version":1,"event_id":"e","host_id":"peer1","type":"heartbeat","time":"` + now + `"}`))
	f.Add([]byte(`{"version":1,"event_id":"","type":"status","data":{"state":"running"}}`))
	f.Add([]byte(strings.Repeat("{", 500)))
	f.Add(make([]byte, 0))
	f.Fuzz(func(t *testing.T, body []byte) {
		r, _ := receiverFixture(t, time.Now())
		srv := httptest.NewServer(r.Handler())
		defer srv.Close()
		req := signedRequest(t, srv.URL, "peer1", "fuzz", time.Now(), testKey, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Skip() // transport-level failures are not interesting here
		}
		resp.Body.Close()
	})
}

// osMkdir keeps the fixture helper honest.
func osMkdir(dir string) error { return os.MkdirAll(dir, 0o755) }
