package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML string scalars such as
// "30s", "5m", or "1h30m". Integer scalars are rejected so units are never
// ambiguous. Zero means the field was not set and a default may apply.
type Duration time.Duration

// UnmarshalYAML parses a positive duration string with line information.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: duration must be a string like \"5m\"", value.Line)
	}
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q: use values like \"30s\", \"5m\", \"1h\"", value.Line, value.Value)
	}
	if parsed <= 0 {
		return fmt.Errorf("line %d: duration %q must be positive", value.Line, value.Value)
	}
	*d = Duration(parsed)
	return nil
}

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalYAML writes the duration in Go string form ("5m") so generated
// configuration stays human-readable instead of leaking nanosecond integers.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}
