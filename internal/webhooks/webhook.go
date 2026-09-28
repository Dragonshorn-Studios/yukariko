// Package webhooks delivers signed deployment notifications to
// operator-configured URLs (issue #65). Delivery is at-least-once from a
// durable store queue: enqueue persists before any network I/O, failures
// back off with jitter and a respected Retry-After, and attempts past the
// cap are abandoned with their last error. Reporting failures never block
// checks or deploys — enqueue is the only coupling to the deploy path.
//
// Payloads are inert state descriptions: they name apps, versions, and
// outcomes, and never select or provide commands. Signing secrets are
// SecretRefs resolved at send time and never logged or stored.
package webhooks

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Event kinds delivered in v1: deployment outcomes only.
const (
	EventDeploymentSucceeded = "deployment.succeeded"
	EventDeploymentFailed    = "deployment.failed"
)

// PayloadVersion is the schema version of the JSON body. Receivers must
// ignore unknown fields; additions are backward compatible, removals or
// re-shapes bump this.
const PayloadVersion = 1

// maxPayloadBytes bounds the rendered JSON body. Deployment state is small;
// a payload nearing this size indicates bad input, not a delivery problem.
const maxPayloadBytes = 16 << 10

// Payload is the v1 webhook body: one deployment outcome for one app.
// Versions are the full from/to forms recorded on the deployment row —
// never the shortened dashboard renderings.
type Payload struct {
	Version    int        `json:"version"`
	Event      string     `json:"event"`
	Host       string     `json:"host,omitempty"`
	App        string     `json:"app"`
	Deployment Deployment `json:"deployment"`
	// Detail is the pipeline's stage summary at the outcome transition.
	Detail string `json:"detail,omitempty"`
	Time   string `json:"time"`
}

// Deployment mirrors the durable deployment row's outcome fields.
type Deployment struct {
	ID          string `json:"id"`
	Cause       string `json:"cause"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at,omitempty"`
	EndedAt     string `json:"ended_at,omitempty"`
}

// sha256Hex is the body digest carried into the signature canonicalization.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Signature canonicalization: hex(HMAC-SHA256(key, timestamp + "." +
// sha256hex(body))). The timestamp binds the signature to a send window and
// the body hash covers the exact bytes on the wire.
func canonical(timestamp string, body []byte) string {
	return timestamp + "." + sha256Hex(body)
}

// rfc3339 renders times for payload fields.
func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
