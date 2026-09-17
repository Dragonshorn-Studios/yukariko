package learn

import (
	"strings"
	"testing"
)

func TestDiffGolden(t *testing.T) {
	t.Parallel()
	oldYAML := "schema_version: 1\napps:\n  - id: web\n    interval: 15m\n    source:\n      mode: registry\n"
	newYAML := "schema_version: 1\napps:\n  - id: web\n    interval: 15m\n    source:\n      mode: git\n      branch: main\n"
	got := Diff("current", "proposed", []byte(oldYAML), []byte(newYAML))
	want := strings.Join([]string{
		"--- current",
		"+++ proposed",
		"@@ -3,4 +3,5 @@", // context extends 3 lines before the change
		"   - id: web",
		"     interval: 15m",
		"     source:",
		"-      mode: registry",
		"+      mode: git",
		"+      branch: main",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Diff =\n%s\nwant\n%s", got, want)
	}
}

func TestDiffIdenticalIsEmpty(t *testing.T) {
	t.Parallel()
	same := []byte("schema_version: 1\napps: []")
	if got := Diff("a", "b", same, same); got != "" {
		t.Errorf("Diff(identical) = %q, want empty", got)
	}
	// A trailing newline is not a difference.
	if got := Diff("a", "b", same, []byte("schema_version: 1\napps: []\n")); got != "" {
		t.Errorf("Diff(trailing newline) = %q, want empty", got)
	}
}

func TestDiffHunkContextMerges(t *testing.T) {
	t.Parallel()
	// Lines 2 and 8 change; with 3 context lines the hunks must merge into
	// one hunk spanning both.
	oldB := "1\n2\n3\n4\n5\n6\n7\n8\n9\n"
	newB := "1\nX\n3\n4\n5\n6\n7\nY\n9\n"
	got := Diff("a", "b", []byte(oldB), []byte(newB))
	// "@@ -" appears once per hunk header.
	if n := strings.Count(got, "@@ -"); n != 1 {
		t.Errorf("hunk count = %d, want 1 (context overlaps):\n%s", n, got)
	}
}
