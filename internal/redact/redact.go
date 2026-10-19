// Package redact removes credentials from values before they are logged or
// exposed over IPC.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// sensitiveParams are query parameter names (lower-case) known to carry credentials.
var sensitiveParams = map[string]bool{
	"api_key":              true,
	"apikey":               true,
	"x-emby-token":         true,
	"x-mediabrowser-token": true,
	"token":                true,
	"access_token":         true,
	"accesstoken":          true,
	"auth":                 true,
	"authorization":        true,
	"password":             true,
	"pw":                   true,
	"key":                  true,
	"sig":                  true,
	"signature":            true,
	"session":              true,
	"sessionid":            true,
}

// URL returns raw with userinfo and credential-carrying query parameters
// removed. Non-URL strings and file:// URLs are returned unchanged apart from
// that processing. If raw cannot be parsed, ok is false and "" is returned.
func URL(raw string) (clean string, ok bool) {
	if raw == "" {
		return "", true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if u.User != nil {
		u.User = nil
	}
	if u.RawQuery != "" {
		q := u.Query()
		changed := false
		for k := range q {
			if sensitiveParams[strings.ToLower(k)] {
				q.Del(k)
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	return u.String(), true
}

var tokenPattern = regexp.MustCompile(`(?i)(token|api_key|password|pw|authorization)(["'=:\s]+)("[^"]*"|[^\s",&]+)`)

// String masks anything that looks like a credential assignment inside s.
// It is a last line of defence for log messages and error strings.
func String(s string) string {
	return tokenPattern.ReplaceAllString(s, `${1}${2}[REDACTED]`)
}

// Secret is a string that never prints its value.
type Secret string

// String implements fmt.Stringer.
func (Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return "[REDACTED]" }

// MarshalJSON prevents accidental serialisation.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalText prevents accidental serialisation (also covers slog).
func (Secret) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// Reveal returns the underlying secret value. Use only at the point of use.
func (s Secret) Reveal() string { return string(s) }
