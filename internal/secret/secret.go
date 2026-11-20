// Package secret wraps one-time join codes, TOTP secrets and similar values
// so that formatting, logging, JSON encoding and journals never reveal them.
package secret

import "strings"

// Secret holds a sensitive string. Every rendering path is redacted; only
// Reveal returns the value, and callers pass it on stdin or to a process
// argument that upstream requires.
type Secret struct{ v string }

// New wraps value.
func New(value string) Secret { return Secret{v: strings.TrimSpace(value)} }

// Reveal returns the underlying value.
func (s Secret) Reveal() string { return s.v }

// Empty reports whether no value is held.
func (s Secret) Empty() bool { return s.v == "" }

func (s Secret) String() string {
	if s.v == "" {
		return "<empty>"
	}
	return "<redacted>"
}

// GoString keeps %#v redacted as well.
func (s Secret) GoString() string { return s.String() }

// MarshalJSON never encodes the value.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + s.String() + `"`), nil }

// MarshalText never encodes the value (TOML, YAML, ...).
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Redact replaces every occurrence of the secret values in text.
func Redact(text string, secrets ...Secret) string {
	for _, s := range secrets {
		if len(s.v) >= 4 {
			text = strings.ReplaceAll(text, s.v, "<redacted>")
		}
	}
	return text
}
