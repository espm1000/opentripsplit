// Package ledger holds the money logic: currencies, amount parsing, expense
// splitting, balances and settle-up. It is pure (no I/O) so it can be tested
// exhaustively.
package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// MaxAmount caps any single amount (in minor units). It keeps every
// intermediate product in Split well inside int64.
const MaxAmount int64 = 1_000_000_000_000

// Currency is an ISO 4217 code and the number of digits after the decimal
// point in its minor unit.
type Currency struct {
	Code     string
	Decimals int
}

// Currencies is the list a group can choose from.
var Currencies = []Currency{
	{"AUD", 2}, {"BHD", 3}, {"BRL", 2}, {"CAD", 2}, {"CHF", 2}, {"CLP", 0},
	{"CNY", 2}, {"CZK", 2}, {"DKK", 2}, {"EUR", 2}, {"GBP", 2}, {"HKD", 2},
	{"HUF", 2}, {"IDR", 2}, {"ILS", 2}, {"INR", 2}, {"ISK", 0}, {"JOD", 3},
	{"JPY", 0}, {"KRW", 0}, {"KWD", 3}, {"MXN", 2}, {"MYR", 2}, {"NOK", 2},
	{"NZD", 2}, {"OMR", 3}, {"PHP", 2}, {"PLN", 2}, {"SEK", 2}, {"SGD", 2},
	{"THB", 2}, {"TRY", 2}, {"TWD", 2}, {"USD", 2}, {"VND", 0}, {"ZAR", 2},
}

// LookupCurrency returns the currency with the given code.
func LookupCurrency(code string) (Currency, bool) {
	for _, c := range Currencies {
		if c.Code == code {
			return c, true
		}
	}
	return Currency{}, false
}

var (
	ErrAmountFormat = errors.New("enter a number like 12.50")
	ErrAmountRange  = errors.New("amount is too large")
)

// ParseAmount parses a non-negative decimal string such as "12.5" into minor
// units with the given number of decimals ("12.5", 2 → 1250). It never uses
// floating point.
func ParseAmount(s string, decimals int) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, ErrAmountFormat
	}
	if hasDot && decimals == 0 {
		return 0, errors.New("use a whole number, this currency has no decimal places")
	}
	if hasDot && frac == "" {
		return 0, ErrAmountFormat
	}
	if len(frac) > decimals {
		return 0, fmt.Errorf("use at most %d decimal places", decimals)
	}
	if !allDigits(whole) || !allDigits(frac) {
		return 0, ErrAmountFormat
	}
	frac += strings.Repeat("0", decimals-len(frac))

	var v int64
	for _, r := range whole + frac {
		v = v*10 + int64(r-'0')
		if v > MaxAmount {
			return 0, ErrAmountRange
		}
	}
	return v, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// FormatAmount renders minor units as a decimal string (1250, 2 → "12.50").
func FormatAmount(v int64, decimals int) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	if decimals == 0 {
		return fmt.Sprintf("%s%d", sign, v)
	}
	p := int64(1)
	for range decimals {
		p *= 10
	}
	return fmt.Sprintf("%s%d.%0*d", sign, v/p, decimals, v%p)
}
