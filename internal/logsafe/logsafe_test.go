package logsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestString(t *testing.T) {
	long := strings.Repeat("a", MaxLen+50)
	// A 3-byte rune straddling the cut must not be split.
	straddle := strings.Repeat("a", MaxLen-1) + "€" + "tail"

	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "access_denied", "access_denied"},
		{"empty", "", ""},
		{"newlines", "a\nb\r\nc", `a\nb\r\nc`},
		{"forged line", "x\ntime=2026-01-01 level=INFO msg=ok", `x\ntime=2026-01-01 level=INFO msg=ok`},
		// The cut happens after line breaks are escaped, so it counts them as 2 bytes.
		{"escaped then cut", strings.Repeat("\n", MaxLen), strings.Repeat(`\n`, MaxLen/2) + "…"},
		{"exactly max", long[:MaxLen], long[:MaxLen]},
		{"too long", long, long[:MaxLen] + "…"},
		{"rune at cut", straddle, straddle[:MaxLen-1] + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.in)
			if got != tc.want {
				t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("String(%q) is not valid UTF-8: %q", tc.in, got)
			}
		})
	}
}
