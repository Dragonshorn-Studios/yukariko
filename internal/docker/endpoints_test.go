package docker

import (
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
				{Name: "rootless", Host: "unix:///run/user/1000/docker.sock"},
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
				{Name: "rootless", Host: "unix:///run/user/1000/docker.sock"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseContexts([]byte(tc.stdout))
			if len(got) != len(tc.want) {
				t.Fatalf("endpoints = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("endpoints[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestEndpointFlags(t *testing.T) {
	t.Parallel()
	if flags := (Endpoint{}).Flags(); flags != nil {
		t.Errorf("default endpoint flags = %v, want nil", flags)
	}
	flags := Endpoint{Name: "rootless"}.Flags()
	if len(flags) != 2 || flags[0] != "--context" || flags[1] != "rootless" {
		t.Errorf("context flags = %v", flags)
	}
}
