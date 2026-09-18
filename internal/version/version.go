package version

import "runtime/debug"

// These are overridable at link time:
//
//	go build -ldflags "-X github.com/Dragonshorn-Studios/yukariko/internal/version.Version=..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// readBuildInfo is swapped in tests.
var readBuildInfo = debug.ReadBuildInfo

// init fills fields that still hold their defaults from embedded build
// information, so `go install module@version` reports its version and a
// plain `go build` from a checkout reports its revision. Link-time flags
// always win.
func init() {
	enrichFromBuildInfo()
}

func enrichFromBuildInfo() {
	bi, ok := readBuildInfo()
	if !ok || bi == nil {
		return
	}
	var rev, when string
	local := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev, local = s.Value, true
		case "vcs.time":
			when = s.Value
		}
	}
	// Only a module install (go install module@version) reports a real
	// Main.Version. Local checkouts synthesize one ("(devel)" or e.g.
	// "v1.0.0+dirty"), so they keep "dev" and report their revision.
	if !local {
		if v := bi.Main.Version; Version == "dev" && v != "" && v != "(devel)" {
			Version = v
		}
	}
	if Commit == "none" && rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		Commit = rev
	}
	if Date == "unknown" && when != "" {
		Date = when
	}
}

// String returns the version shown by --version.
func String() string {
	s := Version
	if Commit != "" && Commit != "none" {
		s += " commit=" + Commit
	}
	if Date != "" && Date != "unknown" {
		s += " date=" + Date
	}
	return s
}
