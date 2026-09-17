package learn

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// diffContext is the number of unchanged context lines shown around each
// unified-diff hunk.
const diffContext = 3

// maxDiffCell bounds the LCS table (len(old) * len(new)); larger inputs get
// a summary instead of a pathological allocation.
const maxDiffCell = 4 << 20

// Diff renders a classic unified diff between old and new with the given
// labels. Identical inputs return "". Output is deterministic. Inputs that
// are too large for the LCS table produce a summary header instead of hunks.
func Diff(oldLabel, newLabel string, oldB, newB []byte) string {
	a := splitLines(oldB)
	b := splitLines(newB)
	if equalLines(a, b) {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n", oldLabel)
	fmt.Fprintf(&sb, "+++ %s\n", newLabel)
	if len(a)*len(b) > maxDiffCell {
		fmt.Fprintf(&sb, "@@ files too large for a line diff (%d vs %d lines) @@\n", len(a), len(b))
		return sb.String()
	}
	ops := diffOps(a, b)
	if len(ops) == 0 {
		return ""
	}
	for _, h := range hunks(ops, diffContext) {
		aStart, bStart := 1, 1
		aLen, bLen := 0, 0
		for _, o := range h {
			switch o.kind {
			case ' ':
				aLen++
				bLen++
			case '-':
				aLen++
			case '+':
				bLen++
			}
		}
		// Find the starting line numbers by replaying the prefix.
		idx := 0
		for _, o := range ops {
			if idx == h[0].pos {
				break
			}
			switch o.kind {
			case ' ':
				aStart++
				bStart++
			case '-':
				aStart++
			case '+':
				bStart++
			}
			idx++
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", aStart, aLen, bStart, bLen)
		for _, o := range h {
			fmt.Fprintf(&sb, "%c%s\n", o.kind, o.line)
		}
	}
	return sb.String()
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
	pos  int // index into the ops slice; used to locate hunk starts
}

// splitLines splits into newline-free lines, dropping the empty element a
// trailing newline produces.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// equalLines compares split line slices; a trailing newline is not a
// difference.
func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diffOps computes the edit script with an LCS DP table. Ties prefer
// deletion, keeping output deterministic.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	w := m + 1
	lcs := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				lcs[i*w+j] = lcs[(i+1)*w+j]
			default:
				lcs[i*w+j] = lcs[i*w+j+1]
			}
		}
	}
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{kind: ' ', line: a[i]})
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			ops = append(ops, diffOp{kind: '-', line: a[i]})
			i++
		default:
			ops = append(ops, diffOp{kind: '+', line: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{kind: '-', line: a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{kind: '+', line: b[j]})
	}
	return ops
}

// hunks groups the edit script into unified hunks with ctx context lines,
// merging runs whose context would overlap.
func hunks(ops []diffOp, ctx int) [][]diffOp {
	changed := make([]int, 0, len(ops))
	for i, o := range ops {
		if o.kind != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	// Build merged intervals [lo, hi) of op indices.
	var intervals [][2]int
	for _, i := range changed {
		lo, hi := i-ctx, i+ctx+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(ops) {
			hi = len(ops)
		}
		if n := len(intervals); n > 0 && lo <= intervals[n-1][1] {
			if hi > intervals[n-1][1] {
				intervals[n-1][1] = hi
			}
			continue
		}
		intervals = append(intervals, [2]int{lo, hi})
	}
	out := make([][]diffOp, 0, len(intervals))
	for _, iv := range intervals {
		h := make([]diffOp, 0, iv[1]-iv[0])
		for k := iv[0]; k < iv[1]; k++ {
			o := ops[k]
			o.pos = k
			h = append(h, o)
		}
		out = append(out, h)
	}
	return out
}

// renderYAML marshals cfg with the stable two-space indent used everywhere
// Yukariko writes configuration.
func renderYAML(cfg interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
