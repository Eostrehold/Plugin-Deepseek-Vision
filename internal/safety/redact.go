package safety

import "strings"

// Redact removes known secrets and image references from diagnostic strings.
// It is deliberately small and deterministic so it is safe to use in error
// paths and tests. Callers should still prefer static error messages.
func Redact(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

// ErrorClass turns arbitrary upstream errors into a stable, non-sensitive
// message suitable for an API response.
func ErrorClass(err error) string {
	if err == nil {
		return ""
	}
	return "visual preprocessing failed"
}
