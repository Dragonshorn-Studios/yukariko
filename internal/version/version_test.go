package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestStringFallback(t *testing.T) {
	// A build from a git checkout enriches commit/date via build info;
	// the version itself must stay "dev" unless injected at link time.
	if got := String(); !strings.HasPrefix(got, "dev") {
		t.Fatalf("default String() = %q, want a dev build", got)
	}
}

func TestStringIncludesBuildMetadata(t *testing.T) {
	origV, origC, origD := Version, Commit, Date
	t.Cleanup(func() {
		Version, Commit, Date = origV, origC, origD
	})
	Version = "1.0.0"
	Commit = "abc123"
	Date = "2026-09-17"
	got := String()
	if got != "1.0.0 commit=abc123 date=2026-09-17" {
		t.Fatalf("got %q", got)
	}
}

func TestEnrichFromBuildInfo(t *testing.T) {
	origRead := readBuildInfo
	origV, origC, origD := Version, Commit, Date
	t.Cleanup(func() {
		readBuildInfo = origRead
		Version, Commit, Date = origV, origC, origD
	})

	cases := []struct {
		name      string
		version   string // starting Version; empty means the "dev" default
		commit    string // starting Commit; empty means the "none" default
		date      string // starting Date; empty means the "unknown" default
		buildInfo *debug.BuildInfo
		ok        bool
		wantV     string
		wantC     string
		wantD     string
	}{
		{
			name:      "module install reports its version",
			buildInfo: &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}},
			ok:        true,
			wantV:     "v1.2.3",
			wantC:     "none",
			wantD:     "unknown",
		},
		{
			name: "git checkout reports revision and time",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef"},
					{Key: "vcs.time", Value: "2026-09-18T06:00:00Z"},
				},
			},
			ok:    true,
			wantV: "dev",
			wantC: "0123456789ab",
			wantD: "2026-09-18T06:00:00Z",
		},
		{
			name: "dirty checkout keeps dev despite synthesized version",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Version: "v1.0.0+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123456789abcdef"},
					{Key: "vcs.time", Value: "2026-09-18T06:00:00Z"},
				},
			},
			ok:    true,
			wantV: "dev",
			wantC: "0123456789ab",
			wantD: "2026-09-18T06:00:00Z",
		},
		{
			name: "short revision is kept as is",
			buildInfo: &debug.BuildInfo{
				Main:     debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
			},
			ok:    true,
			wantV: "dev",
			wantC: "abc123",
			wantD: "unknown",
		},
		{
			name:    "link-time injection wins over build info",
			version: "injected",
			commit:  "injected",
			date:    "injected",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Version: "v9.9.9"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "ffffffffffffffff"},
					{Key: "vcs.time", Value: "1999-01-01T00:00:00Z"},
				},
			},
			ok:    true,
			wantV: "injected",
			wantC: "injected",
			wantD: "injected",
		},
		{
			name:  "no build info leaves defaults",
			wantV: "dev",
			wantC: "none",
			wantD: "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Version, Commit, Date = "dev", "none", "unknown"
			if tc.version != "" {
				Version = tc.version
			}
			if tc.commit != "" {
				Commit = tc.commit
			}
			if tc.date != "" {
				Date = tc.date
			}
			readBuildInfo = func() (*debug.BuildInfo, bool) { return tc.buildInfo, tc.ok }
			enrichFromBuildInfo()
			if Version != tc.wantV || Commit != tc.wantC || Date != tc.wantD {
				t.Fatalf("got %q/%q/%q, want %q/%q/%q",
					Version, Commit, Date, tc.wantV, tc.wantC, tc.wantD)
			}
		})
	}
}
