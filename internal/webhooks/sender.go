package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
)

// Headers carried by every signed delivery.
const (
	HeaderEvent      = "X-Yukariko-Event"
	HeaderDelivery   = "X-Yukariko-Delivery"
	HeaderTimestamp  = "X-Yukariko-Timestamp"
	HeaderSignature  = "X-Yukariko-Signature"
	signatureVersion = "v1"
)

// Attempt is the outcome of one send. The dispatcher classifies and
// schedules; the sender never retries.
type Attempt struct {
	// Delivered is true when the receiver accepted the POST (2xx).
	Delivered bool
	// Abandon suggests the target considers this delivery permanently
	// gone (410); the dispatcher still enforces its own attempt cap.
	Abandon bool
	// RetryAfter, when the receiver sent one, suggests a floor for the
	// next attempt.
	RetryAfter time.Duration
	// Detail is a bounded, secret-free description for last_error.
	Detail string
}

// Sender posts one delivery to one target. HTTP is stdlib; the client is
// injectable for tests.
type Sender struct {
	// HTTPClient sends the request; nil means a client with no overall
	// timeout — the per-webhook timeout bounds the request context instead.
	HTTPClient *http.Client
	// Now is injectable for tests.
	Now func() time.Time
}

// Send delivers one payload to one webhook target. The secret resolves at
// send time only; a resolution failure is a send failure, never a panic and
// never a plaintext fallback. Static headers from the configuration are
// attached with the computed ones (computed always win on collision).
func (s *Sender) Send(ctx context.Context, hook config.Webhook, event, deliveryID string, body []byte) (Attempt, error) {
	if len(body) > maxPayloadBytes {
		return Attempt{}, fmt.Errorf("payload is %d bytes, over the %d byte cap", len(body), maxPayloadBytes)
	}
	timeout := hook.Timeout.D()
	if timeout <= 0 {
		timeout = config.DefaultWebhookTimeout.D()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	now := s.now()
	ts := strconv.FormatInt(now.Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return Attempt{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderEvent, event)
	req.Header.Set(HeaderDelivery, deliveryID)
	req.Header.Set(HeaderTimestamp, ts)
	if hook.SecretRef != nil {
		secret, rErr := hook.SecretRef.Resolve()
		if rErr != nil {
			return Attempt{}, fmt.Errorf("resolve signing secret: %w", rErr)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(canonical(ts, body)))
		req.Header.Set(HeaderSignature, signatureVersion+"="+hex.EncodeToString(mac.Sum(nil)))
	}
	for _, h := range hook.Headers {
		value := h.Value
		if h.SecretRef != nil {
			resolved, rErr := h.SecretRef.Resolve()
			if rErr != nil {
				return Attempt{}, fmt.Errorf("resolve header %s: %w", h.Name, rErr)
			}
			value = resolved
		}
		if value != "" {
			req.Header.Set(h.Name, value)
		}
	}

	client := s.HTTPClient
	if client == nil {
		// Redirects are answered as-is to the dispatcher's classifier: the
		// signature binds this delivery to this target, and following a
		// 3xx would re-send payload and headers to an unvalidated host.
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	res, err := client.Do(req)
	if err != nil {
		return Attempt{}, fmt.Errorf("post: %w", err)
	}
	defer res.Body.Close()
	// The body is drained bounded and unread: status codes decide.
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return Attempt{Delivered: true}, nil
	}
	att := Attempt{Detail: fmt.Sprintf("receiver answered %d", res.StatusCode)}
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		att.Detail = fmt.Sprintf("receiver answered %d (redirect not followed: the signature binds the delivery to its target)", res.StatusCode)
	}
	if res.StatusCode == http.StatusGone {
		att.Abandon = true
	}
	if ra := res.Header.Get("Retry-After"); ra != "" {
		if secs, pErr := strconv.Atoi(ra); pErr == nil && secs > 0 {
			att.RetryAfter = time.Duration(secs) * time.Second
		} else if when, pErr := http.ParseTime(ra); pErr == nil {
			if d := time.Until(when); d > 0 {
				att.RetryAfter = d
			}
		}
	}
	return att, nil
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
