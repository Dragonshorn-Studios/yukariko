package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"
)

// Header names carried by every signed report.
const (
	HeaderHost      = "X-Yukariko-Host"
	HeaderEvent     = "X-Yukariko-Event"
	HeaderTimestamp = "X-Yukariko-Timestamp"
	HeaderSignature = "X-Yukariko-Signature"
)

// Receiver accepts authenticated status reports from allowlisted Yukariko
// peers. It shares nothing with the read-only dashboard API: this is the
// only write endpoint in the product, it accepts state descriptions only,
// and every payload field is inert by construction.
type Receiver struct {
	// Inbound is the validated inbound-reporting configuration.
	Inbound config.InboundReporting
	// Keys resolves allowlisted host key material at request time via
	// SecretRefs; values are used in-memory and never logged or stored.
	Keys func(hostID string) ([][]byte, bool)
	// Store persists dedup state, remote projections, and events.
	Store *store.Store
	// Now is injectable for tests.
	Now func() time.Time

	mu      sync.Mutex
	windows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

// NewReceiver builds a Receiver from validated config.
func NewReceiver(inbound config.InboundReporting, keys func(string) ([][]byte, bool), st *store.Store) *Receiver {
	return &Receiver{Inbound: inbound, Keys: keys, Store: st, windows: map[string]*rateWindow{}}
}

// Handler returns the http handler mounted by the daemon (separate from the
// read-only dashboard API).
func (r *Receiver) Handler() http.Handler {
	return http.HandlerFunc(r.serve)
}

func (r *Receiver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// rejection carries the HTTP status and a safe, static message.
type rejection struct {
	status  int
	message string
}

func (r *Receiver) reject(status int, format string, args ...any) *rejection {
	return &rejection{status: status, message: fmt.Sprintf(format, args...)}
}

func (rej *rejection) Error() string { return rej.message }

func (r *Receiver) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		r.reply(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	hostID := req.Header.Get(HeaderHost)
	if err := r.authorizeTransport(req, hostID); err != nil {
		r.replyError(w, err)
		return
	}
	if err := r.rateLimit(hostID); err != nil {
		r.replyError(w, err)
		return
	}

	body, rerr := readBody(req, r.maxBody())
	if rerr != nil {
		r.replyError(w, rerr)
		return
	}
	sigErr := r.verifySignature(hostID, req.Header, body)
	if sigErr != nil {
		r.replyError(w, sigErr)
		return
	}

	env, err := DecodeEnvelope(body)
	if err != nil {
		r.reply(w, http.StatusBadRequest, err.Error())
		return
	}
	if env.HostID != hostID {
		r.reply(w, http.StatusUnauthorized, "host mismatch")
		return
	}
	sendTime, err := time.Parse(time.RFC3339, env.Time)
	if err != nil {
		r.reply(w, http.StatusBadRequest, "envelope time is not RFC3339")
		return
	}
	if err := r.checkClock(sendTime); err != nil {
		r.replyError(w, err)
		return
	}

	dup, err := r.Store.SeenInboundEvent(ctx(req), hostID, env.EventID, r.now())
	if err != nil {
		r.reply(w, http.StatusInternalServerError, "dedup store failed")
		return
	}
	if dup {
		r.reply(w, http.StatusConflict, "duplicate event")
		return
	}

	if err := r.persist(ctx(req), hostID, env, sendTime); err != nil {
		r.reply(w, http.StatusInternalServerError, "persist failed")
		return
	}
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, `{"accepted":true}`)
}

// authorizeTransport enforces HTTPS policy, the allowlist, and the required
// headers. It returns generic errors: an unknown host and a bad MAC are
// indistinguishable to the caller until the MAC check, and even then the
// message never names keys.
func (r *Receiver) authorizeTransport(req *http.Request, hostID string) error {
	if r.Inbound.IsRequiredTLS() && req.TLS == nil {
		return r.reject(http.StatusBadRequest, "reports must arrive over HTTPS")
	}
	if hostID == "" || req.Header.Get(HeaderEvent) == "" ||
		req.Header.Get(HeaderTimestamp) == "" || req.Header.Get(HeaderSignature) == "" {
		return r.reject(http.StatusBadRequest, "missing report headers")
	}
	keys, known := r.Keys(hostID)
	if !known || len(keys) == 0 {
		return r.reject(http.StatusUnauthorized, "unknown host")
	}
	if ct := req.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return r.reject(http.StatusUnsupportedMediaType, "content type must be application/json")
	}
	return nil
}

// verifySignature checks the timestamp window and the constant-time MAC
// over every allowlisted key (rotation: any current key validates).
func (r *Receiver) verifySignature(hostID string, header http.Header, body []byte) error {
	ts := header.Get(HeaderTimestamp)
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return r.reject(http.StatusBadRequest, "timestamp is not a UNIX-seconds string")
	}
	if err := r.checkClock(time.Unix(secs, 0)); err != nil {
		return err
	}
	keys, known := r.Keys(hostID)
	if !known {
		return r.reject(http.StatusUnauthorized, "unknown host")
	}
	signature := header.Get(HeaderSignature)
	for _, key := range keys {
		if Verify(key, signature, hostID, header.Get(HeaderEvent), ts, body) {
			return nil
		}
	}
	return r.reject(http.StatusUnauthorized, "signature verification failed")
}

func (r *Receiver) checkClock(sendTime time.Time) error {
	skew := r.Inbound.ClockSkew.D()
	if skew <= 0 {
		skew = config.DefaultClockSkew.D()
	}
	now := r.now()
	switch {
	case sendTime.Before(now.Add(-skew)):
		return r.reject(http.StatusUnprocessableEntity, "report is stale beyond the clock window")
	case sendTime.After(now.Add(skew)):
		return r.reject(http.StatusUnprocessableEntity, "report timestamp is in the future")
	}
	return nil
}

func (r *Receiver) rateLimit(hostID string) error {
	limit := r.Inbound.RateLimit.Events
	if limit <= 0 {
		limit = config.DefaultRateLimitEvents
	}
	per := r.Inbound.RateLimit.Per.D()
	if per <= 0 {
		per = config.DefaultRateLimitPer.D()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	win := r.windows[hostID]
	now := r.now()
	if win == nil || now.Sub(win.start) >= per {
		win = &rateWindow{start: now}
		r.windows[hostID] = win
	}
	win.count++
	if win.count > limit {
		return r.reject(http.StatusTooManyRequests, "rate limit exceeded for host")
	}
	return nil
}

func (r *Receiver) maxBody() int64 {
	if r.Inbound.MaxBodyBytes > 0 {
		return int64(r.Inbound.MaxBodyBytes)
	}
	return int64(config.DefaultMaxBodyBytes)
}

func readBody(req *http.Request, max int64) ([]byte, error) {
	if req.ContentLength > max {
		return nil, &rejection{http.StatusRequestEntityTooLarge, "body exceeds the maximum size"}
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, max+1))
	if err != nil {
		return nil, &rejection{http.StatusBadRequest, "body could not be read"}
	}
	if int64(len(body)) > max {
		return nil, &rejection{http.StatusRequestEntityTooLarge, "body exceeds the maximum size"}
	}
	return body, nil
}

// persist records the accepted report: remote host liveness, the reported
// state projection, and an event. Data is inert state descriptions only.
func (r *Receiver) persist(ctx context.Context, hostID string, env Envelope, sendTime time.Time) error {
	now := r.now()
	if err := r.Store.TouchRemoteHost(ctx, hostID, now); err != nil {
		return err
	}
	stateKey := env.Type
	if env.AppID != "" {
		stateKey = env.Type + ":" + env.AppID
	}
	if err := r.Store.UpsertRemoteState(ctx, hostID, stateKey, string(env.Data), now); err != nil {
		return err
	}
	_, err := r.Store.RecordEvent(ctx, store.Event{
		Time:    now,
		AppID:   env.AppID,
		Level:   store.LevelInfo,
		Kind:    "remote_" + env.Type,
		Message: fmt.Sprintf("report from %s (event %s, reported %s)", hostID, env.EventID, sendTime.Format(time.RFC3339)),
	})
	return err
}

func (r *Receiver) reply(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (r *Receiver) replyError(w http.ResponseWriter, err error) {
	var rej *rejection
	if errors.As(err, &rej) {
		r.reply(w, rej.status, rej.message)
		return
	}
	r.reply(w, http.StatusBadRequest, err.Error())
}

func ctx(req *http.Request) context.Context { return req.Context() }
