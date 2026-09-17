package config

import (
	"fmt"
	"strings"
)

// PathError is one validation finding anchored at a configuration path such as
// "apps[1].source.git.branch" or at a YAML source line.
type PathError struct {
	Path string
	Msg  string
}

// Error renders the finding as "path: message" or just the message when no
// path applies.
func (e PathError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

// InvalidError collects every validation finding so users see all problems in
// one pass instead of one per run.
type InvalidError struct {
	Findings []PathError
}

// Error joins all findings, one per line.
func (e *InvalidError) Error() string {
	lines := make([]string, len(e.Findings))
	for i, f := range e.Findings {
		lines[i] = f.Error()
	}
	return strings.Join(lines, "\n")
}

// validator collects path-aware findings.
type validator struct {
	findings []PathError
}

func (v *validator) errorf(path, format string, args ...any) {
	v.findings = append(v.findings, PathError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) err() error {
	if len(v.findings) == 0 {
		return nil
	}
	return &InvalidError{Findings: v.findings}
}
