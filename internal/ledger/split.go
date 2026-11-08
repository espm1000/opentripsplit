package ledger

import (
	"errors"
	"fmt"
	"slices"
)

// SplitType says how an expense is divided between participants.
type SplitType string

const (
	SplitEqual   SplitType = "equal"
	SplitExact   SplitType = "exact"
	SplitPercent SplitType = "percent"
	SplitShares  SplitType = "shares"
)

// SplitTypes lists every split type in display order.
var SplitTypes = []SplitType{SplitEqual, SplitExact, SplitPercent, SplitShares}

// Valid reports whether t is a known split type.
func (t SplitType) Valid() bool { return slices.Contains(SplitTypes, t) }

const (
	// FullPercent is 100% expressed in basis points.
	FullPercent int64 = 10000
	// MaxShareWeight caps a single participant's share weight.
	MaxShareWeight int64 = 1_000_000
)

// Share is one participant's raw input: minor units for exact, basis points
// for percent, a weight for shares, ignored for equal.
type Share struct {
	UserID int64
	Input  int64
}

// Portion is the amount a participant owes for an expense.
type Portion struct {
	UserID int64
	Amount int64
}

var (
	ErrNoParticipants   = errors.New("pick at least one person")
	ErrDuplicateUser    = errors.New("a person is listed twice")
	ErrBadTotal         = errors.New("amount must be greater than zero")
	ErrExactSum         = errors.New("exact amounts must add up to the total")
	ErrPercentSum       = errors.New("percentages must add up to 100")
	ErrShareWeight      = errors.New("shares must be whole numbers greater than zero")
	ErrNegativeAmount   = errors.New("amounts can't be negative")
	ErrUnknownSplitType = errors.New("unknown split type")
)

// Split divides total (minor units) between the given participants. The
// result is sorted by user ID and always sums to total exactly.
//
// Leftover minor units from rounding go one at a time by largest remainder.
// Ties are broken by user-ID order rotated by rotation (callers pass the
// expense ID), so the same person doesn't always get the extra cent.
func Split(total int64, t SplitType, shares []Share, rotation int64) ([]Portion, error) {
	if total <= 0 || total > MaxAmount {
		return nil, ErrBadTotal
	}
	if len(shares) == 0 {
		return nil, ErrNoParticipants
	}
	sorted := slices.Clone(shares)
	slices.SortFunc(sorted, func(a, b Share) int { return cmpInt(a.UserID, b.UserID) })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].UserID == sorted[i-1].UserID {
			return nil, ErrDuplicateUser
		}
	}

	weights := make([]int64, len(sorted))
	switch t {
	case SplitEqual:
		for i := range weights {
			weights[i] = 1
		}
	case SplitExact:
		out := make([]Portion, len(sorted))
		var sum int64
		for i, s := range sorted {
			if s.Input < 0 {
				return nil, ErrNegativeAmount
			}
			if s.Input > total {
				return nil, ErrExactSum
			}
			sum += s.Input
			out[i] = Portion{s.UserID, s.Input}
		}
		if sum != total {
			return nil, ErrExactSum
		}
		return out, nil
	case SplitPercent:
		var sum int64
		for i, s := range sorted {
			if s.Input < 0 || s.Input > FullPercent {
				return nil, ErrPercentSum
			}
			weights[i] = s.Input
			sum += s.Input
		}
		if sum != FullPercent {
			return nil, ErrPercentSum
		}
	case SplitShares:
		for i, s := range sorted {
			if s.Input <= 0 || s.Input > MaxShareWeight {
				return nil, ErrShareWeight
			}
			weights[i] = s.Input
		}
	default:
		return nil, fmt.Errorf("%w %q", ErrUnknownSplitType, t)
	}
	return largestRemainder(total, sorted, weights, rotation), nil
}

func largestRemainder(total int64, shares []Share, weights []int64, rotation int64) []Portion {
	n := int64(len(shares))
	var sumW int64
	for _, w := range weights {
		sumW += w
	}
	out := make([]Portion, n)
	rem := make([]int64, n)
	left := total
	for i, s := range shares {
		num := total * weights[i]
		out[i] = Portion{s.UserID, num / sumW}
		rem[i] = num % sumW
		left -= out[i].Amount
	}

	r := ((rotation % n) + n) % n
	pos := func(i int64) int64 { return ((i - r) + n) % n }
	order := make([]int64, n)
	for i := range order {
		order[i] = int64(i)
	}
	slices.SortFunc(order, func(a, b int64) int {
		if rem[a] != rem[b] {
			return cmpInt(rem[b], rem[a])
		}
		return cmpInt(pos(a), pos(b))
	})
	for _, i := range order[:left] {
		out[i].Amount++
	}
	return out
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
