package report

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DeliveryOutcome classifies one send attempt for the outbox retry policy.
type DeliveryOutcome struct {
	Accepted   bool
	Retryable  bool
	RetryAfter time.Duration
	StatusCode int
	Detail     string
}

// Sender delivers one signed report over HTTP(S). It performs a single
// attempt per call — retry policy lives in the outbox reporter, which backs
// off exponentially instead of hammering a failing receiver.
type Sender struct {
	// HTTPClient is the transport; nil uses one with a 30s timeout.
	HTTPClient *http.Client
	// Now stamps the signature; nil → time.Now. Injectable for tests.
	Now func() time.Time
}

// Send signs and delivers one envelope. The key material is used for this
// request only and never logged or stored.
func (s *Sender) Send(ctx context.Context, endpoint string, key []byte, hostID string, env Envelope) (DeliveryOutcome, error) {
	body, err := json.Marshal(env)
	if err != nil {
		return DeliveryOutcome{}, fmt.Errorf("marshal envelope: %w", err)
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	timestamp := fmt.Sprintf("%d", now.Unix())
	signature := Sign(key, hostID, env.EventID, timestamp, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return DeliveryOutcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderHost, hostID)
	req.Header.Set(HeaderEvent, env.EventID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderSignature, signature)

	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return DeliveryOutcome{Retryable: true, Detail: "transport: " + err.Error()}, nil
	}
	defer resp.Body.Close()

	out := DeliveryOutcome{StatusCode: resp.StatusCode}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		out.Accepted = true
		return out, nil
	case resp.StatusCode == http.StatusConflict:
		// The receiver already holds this event ID (lost-ACK retry): the
		// delivery succeeded semantically.
		out.Accepted = true
		out.Detail = "receiver already had the event (deduplicated)"
		return out, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		out.Retryable = true
		out.Detail = "rate limited"
		if ra, perr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); perr == nil && ra > 0 {
			out.RetryAfter = time.Duration(ra) * time.Second
			out.Detail = fmt.Sprintf("rate limited; Retry-After %s", out.RetryAfter)
		}
	case resp.StatusCode >= 500:
		out.Retryable = true
		out.Detail = "receiver server error"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		out.Detail = "receiver rejected credentials"
	default:
		out.Detail = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}
	return out, nil
}

// BuildEnvelope assembles and validates one outbound envelope with a fresh
// event ID and the current timestamp.
func BuildEnvelope(hostID, typ, appID string, data any, now time.Time) (Envelope, []byte, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return Envelope{}, nil, err
		}
		raw = b
	}
	env := Envelope{
		Version: Version,
		EventID: NewEventID(),
		HostID:  hostID,
		Type:    typ,
		Time:    now.Format(time.RFC3339),
		AppID:   appID,
		Data:    raw,
	}
	// Validate with the same rules the receiver applies.
	if _, err := DecodeEnvelope(mustJSON(env)); err != nil {
		return Envelope{}, nil, err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return Envelope{}, nil, err
	}
	return env, body, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// NewEventID returns a random 128-bit hex event ID.
func NewEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Crypto/rand never fails on supported platforms; fall back to a
		// time-derived ID rather than dropping the event.
		return fmt.Sprintf("evt-%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
