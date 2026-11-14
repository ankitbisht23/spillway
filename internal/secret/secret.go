// Package secret reads secret settings from the environment or from mounted
// files and holds them in a type that never prints its value (SEC-09,
// ADR-0040).
package secret

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// redacted is what a Value prints, logs and marshals as.
const redacted = "[redacted]"

// Value is a secret. Formatting, logging and JSON show "[redacted]"; only
// Reveal returns the value. The value sits behind a pointer, so even fmt
// printing a struct that holds a Value in an unexported field, which skips
// the String method, shows an address rather than the secret.
type Value struct{ v *string }

// New wraps s; an empty s is unset.
func New(s string) Value {
	if s == "" {
		return Value{}
	}
	return Value{v: &s}
}

// Reveal returns the secret, "" when unset. Call it only where the value
// is handed to the system that checks it.
func (s Value) Reveal() string {
	if s.v == nil {
		return ""
	}
	return *s.v
}

// IsSet reports whether the secret has a value.
func (s Value) IsSet() bool { return s.v != nil }

// String is "[redacted]".
func (Value) String() string { return redacted }

// GoString is "[redacted]", for %#v.
func (Value) GoString() string { return redacted }

// Format prints "[redacted]" for every verb.
func (Value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue is "[redacted]" in slog output.
func (Value) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON is the JSON string "[redacted]".
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText is "[redacted]", for encoders that use it.
func (Value) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// FromEnv reads the secret setting name from getenv, or from the file that
// name_FILE names, with one trailing newline trimmed (ADR-0040). Neither
// set: an unset Value. Both set, a file that cannot be read, or an empty
// file: an error. Errors name the setting, never the value.
func FromEnv(getenv func(string) string, name string) (Value, error) {
	v, file := getenv(name), getenv(name+"_FILE")
	switch {
	case v != "" && file != "":
		return Value{}, fmt.Errorf("set %s or %s_FILE, not both", name, name)
	case file != "":
		b, err := os.ReadFile(file) //nolint:gosec // G304: the operator names the file
		if err != nil {
			return Value{}, fmt.Errorf("%s_FILE: cannot read the file %s: %w", name, file, unwrapPathError(err))
		}
		v = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		if v == "" {
			return Value{}, fmt.Errorf("%s_FILE: the file %s is empty", name, file)
		}
	}
	return New(v), nil
}

// Required is FromEnv for a secret the process cannot start without.
func Required(getenv func(string) string, name string) (Value, error) {
	s, err := FromEnv(getenv, name)
	if err == nil && !s.IsSet() {
		err = fmt.Errorf("%s (or %s_FILE) is required", name, name)
	}
	return s, err
}

// unwrapPathError drops the path from an *fs.PathError, which the message
// already names.
func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
