// Package report implements peer-to-peer status reporting: a bounded,
// versioned envelope signed with HMAC-SHA256, an authenticated receiver
// with replay protection (#17), and nothing else that could execute
// commands or touch Docker.
//
// # Envelope v1 signing (documented canonicalization)
//
// A report is a JSON body plus four headers:
//
//	X-Yukariko-Host:      the sending host ID (allowlisted)
//	X-Yukariko-Event:     a unique event ID per report
//	X-Yukariko-Timestamp: the send time as a UNIX-seconds string
//	X-Yukariko-Signature: hex(HMAC-SHA256(key, canonical))
//
// The canonical string is
//
//	hostID + "\n" + eventID + "\n" + timestamp + "\n" + sha256hex(rawBody)
//
// where rawBody is the exact bytes on the wire. The body hash binds the
// content, so any body mutation invalidates the signature; the timestamp is
// covered so it cannot be replayed outside the clock window. Verification
// uses hmac.Equal (constant time).
//
// Envelope fields can describe state only: there is no field that names a
// command, argv, image operation, or any Docker/Git action, and unknown
// fields are rejected.
package report

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Version is the only supported envelope version.
const Version = 1

// Report types.
const (
	TypeHeartbeat  = "heartbeat"
	TypeStatus     = "status"
	TypeHealth     = "health"
	TypeDeployment = "deployment"
)

// Bounds on envelope string fields (bytes), enforced at decode.
const (
	maxEventID     = 128
	maxHostID      = 64
	maxAppID       = 64
	maxVersionStr  = 256
	maxStateStr    = 32
	maxCheckStr    = 16
	maxReasonStr   = 512
	maxTimeStr     = 40
	maxEnvelopeStr = 64 << 10
)

// Envelope is the bounded report payload. Unknown fields are rejected, and
// every string field is length-capped: a report can describe state, never
// carry commands or unbounded data.
type Envelope struct {
	Version int    `json:"version"`
	EventID string `json:"event_id"`
	HostID  string `json:"host_id"`
	Type    string `json:"type"`
	Time    string `json:"time"`
	AppID   string `json:"app_id,omitempty"`
	// Data is type-specific (see Data* types below); decoded strictly.
	Data json.RawMessage `json:"data,omitempty"`
}

// StatusData reports an app's observed runtime state.
type StatusData struct {
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
}

// HealthData reports one health check result.
type HealthData struct {
	Check  string `json:"check"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// DeploymentData reports a deployment outcome (never a request).
type DeploymentData struct {
	Version string `json:"version"`
	Status  string `json:"status"`
}

// DecodeError marks a malformed envelope (distinct from auth/replay
// failures).
type DecodeError struct{ Detail string }

func (e *DecodeError) Error() string { return "malformed envelope: " + e.Detail }

// DecodeEnvelope strictly parses and validates the report body.
func DecodeEnvelope(body []byte) (Envelope, error) {
	var zero Envelope
	if len(body) == 0 {
		return zero, &DecodeError{"empty body"}
	}
	if len(body) > maxEnvelopeStr {
		return zero, &DecodeError{"body exceeds the envelope size bound"}
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	var env Envelope
	if err := dec.Decode(&env); err != nil {
		return zero, &DecodeError{err.Error()}
	}
	// Exactly one JSON document.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return zero, &DecodeError{"expected exactly one JSON document"}
	}
	if env.Version != Version {
		return zero, &DecodeError{fmt.Sprintf("unsupported version %d (supported: %d)", env.Version, Version)}
	}
	if env.EventID == "" || len(env.EventID) > maxEventID {
		return zero, &DecodeError{"event_id missing or too long"}
	}
	if env.HostID == "" || len(env.HostID) > maxHostID {
		return zero, &DecodeError{"host_id missing or too long"}
	}
	if len(env.Time) > maxTimeStr {
		return zero, &DecodeError{"time too long"}
	}
	switch env.Type {
	case TypeHeartbeat:
		if env.AppID != "" {
			return zero, &DecodeError{"heartbeat must not carry app_id"}
		}
	case TypeStatus:
		var d StatusData
		if err := strictData(env.Data, &d); err != nil {
			return zero, err
		}
		if len(d.State) > maxStateStr || len(d.Version) > maxVersionStr {
			return zero, &DecodeError{"status data exceeds bounds"}
		}
	case TypeHealth:
		var d HealthData
		if err := strictData(env.Data, &d); err != nil {
			return zero, err
		}
		if len(d.Check) > maxCheckStr || len(d.State) > maxStateStr || len(d.Reason) > maxReasonStr {
			return zero, &DecodeError{"health data exceeds bounds"}
		}
	case TypeDeployment:
		var d DeploymentData
		if err := strictData(env.Data, &d); err != nil {
			return zero, err
		}
		if d.Version == "" || len(d.Version) > maxVersionStr {
			return zero, &DecodeError{"deployment version missing or too long"}
		}
		if d.Status != "succeeded" && d.Status != "failed" {
			return zero, &DecodeError{"deployment status must be succeeded or failed"}
		}
	default:
		return zero, &DecodeError{fmt.Sprintf("unknown report type %q", env.Type)}
	}
	return env, nil
}

func strictData(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return &DecodeError{"data section is required for this report type"}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return &DecodeError{"data: " + err.Error()}
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return &DecodeError{"data: expected exactly one JSON object"}
	}
	return nil
}

// --- signing ----------------------------------------------------------------

// CanonicalString builds the exact bytes covered by the HMAC. See the
// package documentation; this function is the single source of truth for
// sender and receiver.
func CanonicalString(hostID, eventID, timestamp string, body []byte) string {
	sum := sha256.Sum256(body)
	return hostID + "\n" + eventID + "\n" + timestamp + "\n" + hex.EncodeToString(sum[:])
}

// Sign returns the hex HMAC-SHA256 of the canonical string.
func Sign(key []byte, hostID, eventID, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(CanonicalString(hostID, eventID, timestamp, body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify compares the provided hex signature against the expected HMAC in
// constant time.
func Verify(key []byte, signature, hostID, eventID, timestamp string, body []byte) bool {
	provided, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(CanonicalString(hostID, eventID, timestamp, body)))
	return hmac.Equal(mac.Sum(nil), provided)
}
