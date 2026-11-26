// Package logsafe cleans text from requests before it goes into a log
// attribute.
package logsafe

import (
	"strings"
	"unicode/utf8"
)

// MaxLen is the most bytes of one value String keeps.
const MaxLen = 256

// String returns s with each line break written as a visible \n or \r, cut
// to MaxLen bytes.
//
// slog's TextHandler already quotes values with control characters, so a
// value can't forge a log line. The real risk is size: values such as an
// OAuth error_description have no length limit. The line breaks are
// replaced with strings.ReplaceAll so CodeQL's go/log-injection query can
// recognize this as a sanitizer.
func String(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	if len(s) <= MaxLen {
		return s
	}
	i := MaxLen
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}
