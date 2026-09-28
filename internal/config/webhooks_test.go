package config

import (
	"strings"
	"testing"
	"time"
)

func TestWebhooksValidParsesAndDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`
schema_version: 1
webhooks:
  - name: amadeus
    url: https://amadeus.example.com/hooks/yukariko
    secret_ref: {env: AMADEUS_HOOK_SECRET}
    headers:
      - {name: X-Tenant, value: ops}
  - name: loopback-test
    url: http://127.0.0.1:9090/hook
    timeout: 5s
apps: []
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Webhooks) != 2 {
		t.Fatalf("webhooks = %d, want 2", len(cfg.Webhooks))
	}
	w := cfg.Webhooks[0]
	if w.Name != "amadeus" || w.Timeout != DefaultWebhookTimeout {
		t.Errorf("webhook[0] = %+v; want name kept and default timeout %s", w.Name, DefaultWebhookTimeout.D())
	}
	if cfg.Webhooks[1].Timeout.D() != (5 * time.Second) {
		t.Errorf("webhook[1].timeout = %s, want 5s", cfg.Webhooks[1].Timeout.D())
	}
}

func TestWebhooksInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "duplicate names",
			doc: `
schema_version: 1
webhooks:
  - {name: dup, url: "https://a.example.com/h"}
  - {name: dup, url: "https://b.example.com/h"}
apps: []
`,
			want: `duplicate webhook name "dup"`,
		},
		{
			name: "empty name",
			doc: `
schema_version: 1
webhooks:
  - {url: "https://a.example.com/h"}
apps: []
`,
			want: "webhooks[0].name: is required",
		},
		{
			name: "plain http off loopback",
			doc: `
schema_version: 1
webhooks:
  - {name: amadeus, url: "http://amadeus.lan/hook"}
apps: []
`,
			want: "webhooks[0].url: must use https",
		},
		{
			name: "userinfo in URL",
			doc: `
schema_version: 1
webhooks:
  - {name: amadeus, url: "https://user:pass@amadeus.example.com/hook"}
apps: []
`,
			want: "webhooks[0].url: must not carry userinfo",
		},
		{
			name: "relative URL",
			doc: `
schema_version: 1
webhooks:
  - {name: amadeus, url: "/hooks/yukariko"}
apps: []
`,
			want: `webhooks[0].url: must be an absolute URL`,
		},
		{
			name: "timeout below the floor",
			doc: `
schema_version: 1
webhooks:
  - {name: amadeus, url: "https://a.example.com/h", timeout: 500ms}
apps: []
`,
			want: "webhooks[0].timeout: must be between 1s and 60s",
		},
		{
			name: "timeout above the ceiling",
			doc: `
schema_version: 1
webhooks:
  - {name: amadeus, url: "https://a.example.com/h", timeout: 61s}
apps: []
`,
			want: "webhooks[0].timeout: must be between 1s and 60s",
		},
		{
			name: "header with neither value nor ref",
			doc: `
schema_version: 1
webhooks:
  - name: amadeus
    url: "https://a.example.com/h"
    headers: [{name: X-Tenant}]
apps: []
`,
			want: "webhooks[0].headers[0]: must set either value",
		},
		{
			name: "header with both value and ref",
			doc: `
schema_version: 1
webhooks:
  - name: amadeus
    url: "https://a.example.com/h"
    headers: [{name: X-Tenant, value: ops, secret_ref: {env: X}}]
apps: []
`,
			want: "webhooks[0].headers[0]: must set either value or secret_ref, not both",
		},
		{
			name: "reserved header name",
			doc: `
schema_version: 1
webhooks:
  - name: amadeus
    url: "https://a.example.com/h"
    headers: [{name: X-Yukariko-Signature, value: forged}]
apps: []
`,
			want: "webhooks[0].headers[0].name: is reserved by Yukariko",
		},
		{
			name: "reserved content type",
			doc: `
schema_version: 1
webhooks:
  - name: amadeus
    url: "https://a.example.com/h"
    headers: [{name: content-type, value: text/plain}]
apps: []
`,
			want: "webhooks[0].headers[0].name: is reserved by Yukariko",
		},
		{
			name: "invalid header name",
			doc: `
schema_version: 1
webhooks:
  - name: amadeus
    url: "https://a.example.com/h"
    headers: [{name: "bad header", value: ops}]
apps: []
`,
			want: "webhooks[0].headers[0].name: must be a valid HTTP header name",
		},
		{
			name: "literal secret rejected",
			doc: `
schema_version: 1
webhooks:
  - name: amadeus
    url: "https://a.example.com/h"
    secret: literal-value
apps: []
`,
			want: "field secret not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.doc))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q\n  does not contain %q", err.Error(), tt.want)
			}
		})
	}
}
