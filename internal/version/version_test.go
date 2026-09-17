package version

import "testing"

func TestStringFallback(t *testing.T) {
	if String() != "dev" {
		t.Fatalf("default String() = %q", String())
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
