package config

import (
	"fmt"
	"os"
	"strings"
)

// SecretRef references a secret without ever containing its value. Secrets are
// read from an environment variable or a protected file at the moment a
// consumer needs them; they are never stored in YAML, SQLite, logs, or errors.
// A SecretRef with both Env and File set, or with neither, is invalid.
type SecretRef struct {
	Env  string `yaml:"env,omitempty"`
	File string `yaml:"file,omitempty"`
}

// String renders the reference itself, never a secret value. It is safe for
// logs, errors, and debug output.
func (s SecretRef) String() string {
	switch {
	case s.Env != "":
		return fmt.Sprintf("secretRef(env:%s)", s.Env)
	case s.File != "":
		return fmt.Sprintf("secretRef(file:%s)", s.File)
	default:
		return "secretRef(unset)"
	}
}

// Resolve reads the secret value at call time from the environment or from the
// referenced file. It is never called during configuration loading. File
// contents are trimmed (secret files conventionally end with a newline);
// environment values are used verbatim.
func (s SecretRef) Resolve() (string, error) {
	switch {
	case s.Env != "" && s.File == "":
		v, ok := os.LookupEnv(s.Env)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", s.Env)
		}
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty", s.Env)
		}
		return v, nil
	case s.File != "" && s.Env == "":
		data, err := os.ReadFile(s.File)
		if err != nil {
			return "", fmt.Errorf("read secret file: %w", err)
		}
		v := strings.TrimSpace(string(data))
		if v == "" {
			return "", fmt.Errorf("secret file %s is empty", s.File)
		}
		return v, nil
	default:
		return "", fmt.Errorf("secret reference must set exactly one of env or file, got %s", s)
	}
}

// MarshalYAML keeps refs (never values) when Yukariko writes YAML, such as the
// learn import in issue #7.
func (s SecretRef) MarshalYAML() (any, error) {
	return secretRefYAML{Env: s.Env, File: s.File}, nil
}

type secretRefYAML struct {
	Env  string `yaml:"env,omitempty"`
	File string `yaml:"file,omitempty"`
}
