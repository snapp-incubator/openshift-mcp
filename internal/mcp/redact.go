package mcp

import (
	"net/url"
	"strings"
)

// Redaction removes credential VALUES from an object while leaving everything
// that explains the object intact.
//
// The rule is: hide the value, keep the fact. A container whose env holds
// DB_PASSWORD still shows the variable, its name, and that it is set inline —
// only the literal is replaced. A reference to a Secret (secretKeyRef, tls
// secretName) is not a credential and is never touched, because "which secret
// does this use" is a question the bot must still be able to answer.
//
// This is deliberately name-driven, not entropy-driven. Guessing at "this
// string looks random" would redact image digests, resource versions, pod
// hashes and IDs — all of which are load-bearing for diagnosis.

const redacted = "[redacted]"

// sensitiveParts are substrings that mark a field as holding a credential.
var sensitiveParts = []string{
	"password", "passwd", "passphrase",
	"secret", "token", "credential",
	"apikey", "accesskey", "privatekey", "signingkey", "encryptionkey",
	"clientsecret", "bearer", "sasl", "authorization",
}

// referenceKeys name a Secret rather than carrying one. They are exempt: the
// value is a resource name, and losing it costs real diagnostic ability.
var referenceKeys = map[string]bool{
	"secretname":         true,
	"secretnamespace":    true,
	"secretref":          true,
	"secretkeyref":       true,
	"secretkeyselector":  true,
	"existingsecret":     true,
	"configsecret":       true,
	"tokensecret":        true,
	"passwordsecret":     true,
	"credentialssecret":  true,
	"secretgeneratorref": true,
	"imagepullsecrets":   true,
	"secrets":            true,
}

// sensitiveKey reports whether a field name denotes a credential value.
func sensitiveKey(key string) bool {
	k := normalizeKey(key)
	if referenceKeys[k] {
		return false
	}
	for _, p := range sensitiveParts {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

// normalizeKey lowercases and drops separators so DB_PASSWORD, db-password and
// dbPassword all compare equal.
func normalizeKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	for _, r := range strings.ToLower(k) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// redactObject walks obj in place and returns how many values it replaced.
// The count is reported to the caller so the model can say a value exists but
// is hidden, instead of concluding it is unset.
func redactObject(obj map[string]any) int {
	n := 0
	redactMap(obj, &n)
	return n
}

func redactMap(m map[string]any, n *int) {
	// EnvVar shape: {name: DB_PASSWORD, value: literal}. The sensitive name is
	// in a sibling field, so the generic key rule cannot see it.
	if name, ok := m["name"].(string); ok && sensitiveKey(name) {
		if _, has := m["value"].(string); has {
			m["value"] = redacted
			*n++
		}
	}

	// Route TLS shape: spec.tls.key is an inline PRIVATE KEY, but the field is
	// called "key", which is far too common a name to treat as sensitive on its
	// own. Recognise it by its siblings. The certificate stays — it is public,
	// and expiry and subject are exactly what gets asked about.
	if _, isTLS := m["termination"]; isTLS {
		if v, has := m["key"].(string); has && v != "" {
			m["key"] = redacted
			*n++
		}
	}

	for k, v := range m {
		switch val := v.(type) {
		case map[string]any:
			// A structured value under a sensitive key is a reference
			// (secretKeyRef and friends), not a literal — recurse, never blank.
			redactMap(val, n)
		case []any:
			redactSlice(val, n)
		case string:
			if sensitiveKey(k) {
				if val != "" {
					m[k] = redacted
					*n++
				}
				continue
			}
			if s, changed := redactString(val); changed {
				m[k] = s
				*n++
			}
		default:
			// Numbers and booleans are not credentials worth hiding, except
			// under an unambiguously sensitive key.
			if sensitiveKey(k) && v != nil {
				m[k] = redacted
				*n++
			}
		}
	}
}

func redactSlice(s []any, n *int) {
	for i, v := range s {
		switch val := v.(type) {
		case map[string]any:
			redactMap(val, n)
		case []any:
			redactSlice(val, n)
		case string:
			if r, changed := redactString(val); changed {
				s[i] = r
				*n++
			}
		}
	}
}

// redactString handles credentials that live inside a value rather than behind
// a field name: URL userinfo, and command-line flags. Both are how monitoring
// operator specs (remoteWrite URLs, extraArgs) carry secrets inline.
func redactString(s string) (string, bool) {
	if out, ok := redactFlag(s); ok {
		return out, true
	}
	return redactURL(s)
}

// redactURL rewrites scheme://user:pass@host to scheme://user:[redacted]@host,
// keeping the endpoint visible — the host and path are the diagnostic content.
func redactURL(s string) (string, bool) {
	if !strings.Contains(s, "://") || !strings.Contains(s, "@") {
		return s, false
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.User == nil {
		return s, false
	}
	if _, hasPass := u.User.Password(); !hasPass {
		return s, false
	}
	u.User = url.UserPassword(u.User.Username(), redacted)
	// url.String() percent-encodes the placeholder; undo that so it reads back
	// as the marker rather than as %5Bredacted%5D.
	return strings.Replace(u.String(), url.QueryEscape(redacted), redacted, 1), true
}

// redactFlag handles "-remoteWrite.basicAuth.password=hunter2", the form
// VictoriaMetrics extraArgs and container args use. The flag name survives, so
// it stays visible that the flag is set.
func redactFlag(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "-") {
		return s, false
	}
	name, value, found := strings.Cut(t, "=")
	if !found || value == "" {
		return s, false
	}
	if !sensitiveKey(name) {
		return s, false
	}
	return name + "=" + redacted, true
}
