package version

// These are overridable at link time:
//
//	go build -ldflags "-X github.com/Dragonshorn-Studios/yukariko/internal/version.Version=..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

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
