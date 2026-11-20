package ledger

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestParseAmount(t *testing.T) {
	tests := []struct {
		in       string
		decimals int
		want     int64
		wantErr  bool
	}{
		{"12.50", 2, 1250, false},
		{"12.5", 2, 1250, false},
		{"12", 2, 1200, false},
		{" 0.01 ", 2, 1, false},
		{".5", 2, 50, false},
		{"0", 2, 0, false},
		{"1000", 0, 1000, false},
		{"1.234", 3, 1234, false},
		{"1.2", 3, 1200, false},
		{"0.1", 2, 10, false}, // the classic float trap
		{"1.10", 0, 0, true},  // JPY has no minor unit
		{"1.", 2, 0, true},
		{"", 2, 0, true},
		{".", 2, 0, true},
		{"1.234", 2, 0, true},
		{"-1", 2, 0, true},
		{"+1", 2, 0, true},
		{"1e3", 2, 0, true},
		{"1,000", 2, 0, true},
		{"1.2.3", 2, 0, true},
		{"10000000000.00", 2, MaxAmount, false},
		{"10000000000.01", 2, 0, true},
		{"99999999999999999999999", 0, 0, true},
	}
	for _, tt := range tests {
		got, err := ParseAmount(tt.in, tt.decimals)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseAmount(%q, %d) = %d, %v; want %d, err=%v", tt.in, tt.decimals, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestFormatAmount(t *testing.T) {
	tests := []struct {
		v        int64
		decimals int
		want     string
	}{
		{1250, 2, "12.50"},
		{5, 2, "0.05"},
		{0, 2, "0.00"},
		{-1250, 2, "-12.50"},
		{-5, 2, "-0.05"},
		{1500, 0, "1500"},
		{1234, 3, "1.234"},
		{7, 3, "0.007"},
	}
	for _, tt := range tests {
		if got := FormatAmount(tt.v, tt.decimals); got != tt.want {
			t.Errorf("FormatAmount(%d, %d) = %q, want %q", tt.v, tt.decimals, got, tt.want)
		}
	}
}

func TestParseFormatRoundTrip(t *testing.T) {
	for _, c := range Currencies {
		for _, v := range []int64{0, 1, 9, 10, 99, 100, 101, 123456, MaxAmount} {
			got, err := ParseAmount(FormatAmount(v, c.Decimals), c.Decimals)
			if err != nil || got != v {
				t.Fatalf("%s: round trip of %d gave %d, %v", c.Code, v, got, err)
			}
		}
	}
}

func TestLookupCurrency(t *testing.T) {
	for code, dec := range map[string]int{"USD": 2, "JPY": 0, "KWD": 3} {
		c, ok := LookupCurrency(code)
		if !ok || c.Decimals != dec {
			t.Errorf("LookupCurrency(%s) = %+v, %v", code, c, ok)
		}
	}
	if _, ok := LookupCurrency("XXX"); ok {
		t.Error("LookupCurrency(XXX) found")
	}
	seen := map[string]bool{}
	for _, c := range Currencies {
		if seen[c.Code] {
			t.Errorf("duplicate currency %s", c.Code)
		}
		seen[c.Code] = true
	}
}

func users(ids ...int64) []Share {
	out := make([]Share, len(ids))
	for i, id := range ids {
		out[i] = Share{UserID: id}
	}
	return out
}

func amounts(ps []Portion) map[int64]int64 {
	m := map[int64]int64{}
	for _, p := range ps {
		m[p.UserID] = p.Amount
	}
	return m
}

func TestSplit(t *testing.T) {
	tests := []struct {
		name     string
		total    int64
		typ      SplitType
		shares   []Share
		rotation int64
		want     map[int64]int64
		wantErr  error
	}{
		{"equal even", 900, SplitEqual, users(1, 2, 3), 0, map[int64]int64{1: 300, 2: 300, 3: 300}, nil},
		{"equal leftover rot0", 100, SplitEqual, users(1, 2, 3), 0, map[int64]int64{1: 34, 2: 33, 3: 33}, nil},
		{"equal leftover rot1", 100, SplitEqual, users(1, 2, 3), 1, map[int64]int64{1: 33, 2: 34, 3: 33}, nil},
		{"equal leftover rot2", 100, SplitEqual, users(1, 2, 3), 2, map[int64]int64{1: 33, 2: 33, 3: 34}, nil},
		{"equal two leftovers rot2", 101, SplitEqual, users(1, 2, 3), 2, map[int64]int64{1: 34, 2: 33, 3: 34}, nil},
		{"equal unsorted input", 100, SplitEqual, users(3, 1, 2), 0, map[int64]int64{1: 34, 2: 33, 3: 33}, nil},
		{"equal negative rotation", 100, SplitEqual, users(1, 2, 3), -1, map[int64]int64{1: 33, 2: 33, 3: 34}, nil},
		{"equal one person", 1, SplitEqual, users(7), 5, map[int64]int64{7: 1}, nil},
		{"equal more people than cents", 2, SplitEqual, users(1, 2, 3), 0, map[int64]int64{1: 1, 2: 1, 3: 0}, nil},

		{"exact", 1000, SplitExact, []Share{{1, 250}, {2, 750}}, 0, map[int64]int64{1: 250, 2: 750}, nil},
		{"exact with zero", 1000, SplitExact, []Share{{1, 0}, {2, 1000}}, 0, map[int64]int64{1: 0, 2: 1000}, nil},
		{"exact short", 1000, SplitExact, []Share{{1, 250}, {2, 700}}, 0, nil, ErrExactSum},
		{"exact over", 1000, SplitExact, []Share{{1, -1}, {2, 1001}}, 0, nil, ErrNegativeAmount},
		{"exact one over total", 1000, SplitExact, []Share{{1, 1001}}, 0, nil, ErrExactSum},

		{"percent", 1000, SplitPercent, []Share{{1, 2500}, {2, 7500}}, 0, map[int64]int64{1: 250, 2: 750}, nil},
		{"percent thirds", 100, SplitPercent, []Share{{1, 3333}, {2, 3333}, {3, 3334}}, 0, map[int64]int64{1: 33, 2: 33, 3: 34}, nil},
		{"percent largest remainder", 1001, SplitPercent, []Share{{1, 1000}, {2, 9000}}, 0, map[int64]int64{1: 100, 2: 901}, nil},
		{"percent zero gets nothing", 101, SplitPercent, []Share{{1, 0}, {2, 5000}, {3, 5000}}, 0, map[int64]int64{1: 0, 2: 51, 3: 50}, nil},
		{"percent under 100", 1000, SplitPercent, []Share{{1, 5000}, {2, 4999}}, 0, nil, ErrPercentSum},
		{"percent over 100", 1000, SplitPercent, []Share{{1, 10001}}, 0, nil, ErrPercentSum},

		{"shares", 1000, SplitShares, []Share{{1, 1}, {2, 3}}, 0, map[int64]int64{1: 250, 2: 750}, nil},
		{"shares leftover", 1000, SplitShares, []Share{{1, 1}, {2, 1}, {3, 1}}, 0, map[int64]int64{1: 334, 2: 333, 3: 333}, nil},
		{"shares uneven remainders", 10, SplitShares, []Share{{1, 2}, {2, 1}}, 0, map[int64]int64{1: 7, 2: 3}, nil},
		{"shares zero", 1000, SplitShares, []Share{{1, 0}, {2, 1}}, 0, nil, ErrShareWeight},
		{"shares too big", 1000, SplitShares, []Share{{1, MaxShareWeight + 1}}, 0, nil, ErrShareWeight},

		{"zero total", 0, SplitEqual, users(1), 0, nil, ErrBadTotal},
		{"negative total", -5, SplitEqual, users(1), 0, nil, ErrBadTotal},
		{"total too big", MaxAmount + 1, SplitEqual, users(1), 0, nil, ErrBadTotal},
		{"no participants", 100, SplitEqual, nil, 0, nil, ErrNoParticipants},
		{"duplicate", 100, SplitEqual, users(1, 1), 0, nil, ErrDuplicateUser},
		{"bad type", 100, SplitType("magic"), users(1), 0, nil, ErrUnknownSplitType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Split(tt.total, tt.typ, tt.shares, tt.rotation)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m := amounts(got); !mapsEqual(m, tt.want) {
				t.Fatalf("got %v, want %v", m, tt.want)
			}
			if !slices.IsSortedFunc(got, func(a, b Portion) int { return cmpInt(a.UserID, b.UserID) }) {
				t.Fatalf("result not sorted by user ID: %v", got)
			}
		})
	}
}

func mapsEqual(a, b map[int64]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// TestSplitProperties checks on random inputs that every split sums to the
// total and that no one is more than one minor unit off their exact share.
func TestSplitProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 5000 {
		n := 1 + rng.IntN(12)
		total := 1 + rng.Int64N(1_000_000)
		shares := make([]Share, n)
		for i := range shares {
			shares[i] = Share{UserID: int64(i + 1), Input: 1 + rng.Int64N(20)}
		}
		typ := SplitShares
		if rng.IntN(2) == 0 {
			typ = SplitEqual
		}
		got, err := Split(total, typ, shares, rng.Int64())
		if err != nil {
			t.Fatal(err)
		}
		var sum, sumW int64
		for _, s := range shares {
			if typ == SplitEqual {
				sumW++
			} else {
				sumW += s.Input
			}
		}
		for i, p := range got {
			sum += p.Amount
			w := int64(1)
			if typ == SplitShares {
				w = shares[i].Input
			}
			floor := total * w / sumW
			if p.Amount != floor && p.Amount != floor+1 {
				t.Fatalf("user %d got %d, exact share floor is %d", p.UserID, p.Amount, floor)
			}
		}
		if sum != total {
			t.Fatalf("split of %d summed to %d", total, sum)
		}
	}
}

func TestSplitRotationIsFair(t *testing.T) {
	extra := map[int64]int{}
	for exp := range int64(30) {
		got, err := Split(100, SplitEqual, users(1, 2, 3), exp)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range got {
			if p.Amount == 34 {
				extra[p.UserID]++
			}
		}
	}
	for _, id := range []int64{1, 2, 3} {
		if extra[id] != 10 {
			t.Fatalf("extra cents per user = %v, want 10 each", extra)
		}
	}
}

func TestBalances(t *testing.T) {
	expenses := []Expense{
		{PayerID: 1, Amount: 900, Portions: []Portion{{1, 300}, {2, 300}, {3, 300}}},
		{PayerID: 2, Amount: 300, Portions: []Portion{{1, 150}, {2, 150}}},
	}
	settlements := []Settlement{{From: 3, To: 1, Amount: 100}}
	got := Balances(expenses, settlements)
	// 1: +900 −300 −150 −100 = 350; 2: +300 −300 −150 = −150; 3: −300 +100 = −200
	want := map[int64]int64{1: 350, 2: -150, 3: -200}
	if !mapsEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSettleUp(t *testing.T) {
	tests := []struct {
		name string
		in   map[int64]int64
		want []Payment
	}{
		{"empty", map[int64]int64{}, nil},
		{"all zero", map[int64]int64{1: 0, 2: 0}, nil},
		{"one pair", map[int64]int64{1: 500, 2: -500}, []Payment{{2, 1, 500}}},
		{"two debtors", map[int64]int64{1: 350, 2: -150, 3: -200}, []Payment{{3, 1, 200}, {2, 1, 150}}},
		{"tie broken by id", map[int64]int64{1: 100, 2: 100, 3: -100, 4: -100}, []Payment{{3, 1, 100}, {4, 2, 100}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SettleUp(tt.in); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSettleUpProperties builds random groups and checks that balances sum to
// zero and that the suggested payments clear them in at most n−1 transfers.
func TestSettleUpProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for range 2000 {
		n := 2 + rng.IntN(10)
		var expenses []Expense
		for range 1 + rng.IntN(15) {
			k := 1 + rng.IntN(n)
			total := 1 + rng.Int64N(100_000)
			p, err := Split(total, SplitEqual, users(int64Range(int64(k))...), rng.Int64())
			if err != nil {
				t.Fatal(err)
			}
			expenses = append(expenses, Expense{PayerID: 1 + rng.Int64N(int64(n)), Amount: total, Portions: p})
		}
		bal := Balances(expenses, nil)
		var sum int64
		nonZero := 0
		for _, v := range bal {
			sum += v
			if v != 0 {
				nonZero++
			}
		}
		if sum != 0 {
			t.Fatalf("balances sum to %d", sum)
		}

		pays := SettleUp(bal)
		if nonZero > 0 && len(pays) > nonZero-1 {
			t.Fatalf("%d payments for %d non-zero balances", len(pays), nonZero)
		}
		var settlements []Settlement
		for _, p := range pays {
			if p.Amount <= 0 || p.From == p.To {
				t.Fatalf("bad payment %+v", p)
			}
			settlements = append(settlements, Settlement(p))
		}
		for id, v := range Balances(expenses, settlements) {
			if v != 0 {
				t.Fatalf("user %d left with %d after settle-up", id, v)
			}
		}
	}
}

func int64Range(n int64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(i + 1)
	}
	return out
}
