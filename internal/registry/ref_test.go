package registry

import (
	"testing"
)

func TestParseRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in    string
		want  Ref
		str   string
		errOK bool
	}{
		{in: "nginx", want: Ref{Registry: "docker.io", Repository: "library/nginx", Tag: "latest"}, str: "docker.io/library/nginx:latest"},
		{in: "nginx:1.27", want: Ref{Registry: "docker.io", Repository: "library/nginx", Tag: "1.27"}, str: "docker.io/library/nginx:1.27"},
		{in: "team/app", want: Ref{Registry: "docker.io", Repository: "team/app", Tag: "latest"}, str: "docker.io/team/app:latest"},
		{in: "ghcr.io/team/app:v2", want: Ref{Registry: "ghcr.io", Repository: "team/app", Tag: "v2"}, str: "ghcr.io/team/app:v2"},
		{in: "127.0.0.1:5000/app:dev", want: Ref{Registry: "127.0.0.1:5000", Repository: "app", Tag: "dev"}, str: "127.0.0.1:5000/app:dev"},
		{
			in:   "app@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			want: Ref{Registry: "docker.io", Repository: "library/app", Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"},
			str:  "docker.io/library/app@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
		{
			in:   "ghcr.io/team/app:1.0@sha256:2222222222222222222222222222222222222222222222222222222222222222",
			want: Ref{Registry: "ghcr.io", Repository: "team/app", Tag: "1.0", Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222"},
			str:  "ghcr.io/team/app:1.0@sha256:2222222222222222222222222222222222222222222222222222222222222222",
		},
		{in: "", errOK: true},
		{in: "   ", errOK: true},
		{in: "app@sha256:zzz", errOK: true},
		{in: "app:bad tag!", errOK: true},
		{in: ":tag", errOK: true},
		{in: "UPPER/repo", errOK: true},
	}
	for _, tc := range tests {
		got, err := ParseRef(tc.in)
		if tc.errOK {
			if err == nil {
				t.Errorf("ParseRef(%q) = %+v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRef(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if got.String() != tc.str {
			t.Errorf("ParseRef(%q).String() = %q, want %q", tc.in, got.String(), tc.str)
		}
	}
}

func TestRefEndpoint(t *testing.T) {
	t.Parallel()
	r, err := ParseRef("nginx")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.apiEndpoint(nil); got != "https://registry-1.docker.io" {
		t.Errorf("docker.io endpoint = %q", got)
	}
	local, _ := ParseRef("127.0.0.1:5000/app")
	if got := local.apiEndpoint(nil); got != "http://127.0.0.1:5000" {
		t.Errorf("loopback endpoint = %q, want plain http", got)
	}
	ovr, _ := ParseRef("ghcr.io/team/app")
	if got := ovr.apiEndpoint(map[string]string{"ghcr.io": "http://127.0.0.1:8080"}); got != "http://127.0.0.1:8080" {
		t.Errorf("override endpoint = %q", got)
	}
}
