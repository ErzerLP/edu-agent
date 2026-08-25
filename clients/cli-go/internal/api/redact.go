package api

import "strings"

func Redact(text string, sensitive ...string) string {
	redacted := text
	for _, value := range sensitive {
		if value != "" {
			redacted = strings.ReplaceAll(redacted, value, "[redacted]")
		}
	}
	return redacted
}
