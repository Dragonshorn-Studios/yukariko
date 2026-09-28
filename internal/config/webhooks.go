package config

// Webhook is one operator-configured outbound notification target
// (issue #65). Targets are trusted endpoints the operator registers in
// YAML — the HTTP surface stays read-only, so there is no other way in.
// Delivery is at-least-once from a durable queue; see internal/webhook.
type Webhook struct {
	// Name is a unique human-readable identifier used in logs, CLI output,
	// and the X-Yukariko-Delivery header.
	Name string `yaml:"name"`
	// URL is the https (or loopback http, a test affordance) endpoint that
	// receives signed POSTs on deployment success/failure.
	URL string `yaml:"url"`
	// SecretRef optionally names the HMAC signing key. It is a reference
	// only, resolved at send time, and never logged or stored.
	SecretRef *SecretRef `yaml:"secret_ref,omitempty"`
	// Timeout bounds one delivery attempt. Zero takes the default.
	Timeout Duration `yaml:"timeout,omitempty"`
	// Headers are optional static headers attached to every delivery;
	// values are plain or secret_ref, exactly one.
	Headers []WebhookHeader `yaml:"headers,omitempty"`
}

// WebhookHeader is one static delivery header: exactly one of value or
// secret_ref (the same contract as health-probe headers).
type WebhookHeader struct {
	Name      string     `yaml:"name"`
	Value     string     `yaml:"value,omitempty"`
	SecretRef *SecretRef `yaml:"secret_ref,omitempty"`
}

// ApplyDefaults fills unset optional webhook fields.
func (w *Webhook) ApplyDefaults() {
	if w.Timeout == 0 {
		w.Timeout = DefaultWebhookTimeout
	}
}
