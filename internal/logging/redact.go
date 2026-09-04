package logging

import (
	"fmt"
	"regexp"
	"strings"
)

var sensitivePattern = regexp.MustCompile(`(?i)(password|passwd|secret|key|token|authorization|cookie|csrf)`)
var connectionPattern = regexp.MustCompile(`ss://[^\s"']+`)

func IsSensitiveKey(key string) bool { return sensitivePattern.MatchString(key) }

func RedactText(value string) string {
	value = connectionPattern.ReplaceAllString(value, "[REDACTED_URI]")
	if strings.Contains(value, ">>>") && strings.Contains(value, "traffic") {
		return value
	}
	return value
}

func RedactValue(key string, value any) any {
	if IsSensitiveKey(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case string:
		return RedactText(typed)
	case fmt.Stringer:
		return RedactText(typed.String())
	default:
		return value
	}
}
