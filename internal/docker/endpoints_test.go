package docker

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseContexts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		stdout string
		want   []Endpoint
	}{
		{
			name:   "empty output keeps the implicit default",
			stdout: "",
			want:   []Endpoint{{}},
		},
		{
			name: "default context folds into the implicit entry",
			stdout: `{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}` + "\n" +
				`{"Name":"rootless","Endpoints":{"docker":{"Host":"unix:///run/user/1000/docker.sock"}}}` + "\n",
			want: []Endpoint{
				{},
				{Name: "rootless", Host: "unix:///run/user/1000/docker.sock", Flags: []string{"--context", "rootless"}},
			},
		},
		{
			name: "remote endpoints are excluded",
			stdout: `{"Name":"remote","Endpoints":{"docker":{"Host":"ssh://user@box"}}}` + "\n" +
				`{"Name":"remote-tcp","Endpoints":{"docker":{"Host":"tcp://10.0.0.5:2375"}}}` + "\n",
			want: []Endpoint{{}},
		},
		{
			name: "duplicate hosts are collapsed",
			stdout: `{"Name":"default","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}` + "\n" +
				`{"Name":"alias","Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}}}` + "\n",
			want: []Endpoint{{}},
		},
		{
			name: "unparsable lines are ignored",
			stdout: "not json\n" +
				`{"Name":"rootless","Endpoints":{"docker":{"Host":"unix:///run/user/1000/docker.sock"}}}` + "\n",
			want: []Endpoint{
				{},
				{Name: "rootless", Host: "unix:///run/user/1000/docker.sock", Flags: []string{"--context", "rootless"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseContexts([]byte(tc.stdout))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("endpoints = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Rootless daemons need no Docker context: their sockets under /run/user
// are the ground truth, addressed directly by -H (the invoking root or
// service user usually has no context entry for them).
func TestSocketsUnder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, uid := range []string{"1000", "1001"} {
		if err := os.MkdirAll(filepath.Join(dir, uid), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l1, err := net.Listen("unix", filepath.Join(dir, "1000", "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l1.Close() })
	l2, err := net.Listen("unix", filepath.Join(dir, "1001", "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l2.Close() })
	// A plain file named docker.sock must be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "1002"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1002", "docker.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got := socketsUnder(dir)
	want := []Endpoint{
		{Name: "rootless-1000", Host: "unix://" + filepath.Join(dir, "1000", "docker.sock"), Flags: []string{"-H", "unix://" + filepath.Join(dir, "1000", "docker.sock")}},
		{Name: "rootless-1001", Host: "unix://" + filepath.Join(dir, "1001", "docker.sock"), Flags: []string{"-H", "unix://" + filepath.Join(dir, "1001", "docker.sock")}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sockets = %+v, want %+v", got, want)
	}

	if all := socketsUnder(filepath.Join(dir, "missing")); all != nil {
		t.Fatalf("missing dir = %+v, want nil", all)
	}
}
