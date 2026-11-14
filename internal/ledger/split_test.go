package ledger

import (
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"text/tabwriter"
)

// The split tests check the arithmetic two ways:
//
//   - Worked examples whose expected amounts are derived by hand in the
//     comments, so a reviewer can check every number with a calculator.
//   - verifySplit, an independent checker that recomputes each exact share
//     with math/big and confirms Split returned the one result the
//     largest-remainder rule allows. It runs on thousands of random inputs
//     and at the int64 limits.
//
// The TestRoundRobin* tests pin down how leftover units are handed out when
// remainders tie: in user-ID order, starting at sorted position
// rotation mod n (callers pass the expense ID) and wrapping around.
//
// The tests log their working: each split's derivation table, and summaries
// for the loops. go test shows the log for failing tests only; to see it
// for passing ones, run for example
//
//	go test -v -run 'TestSplitWorkedExamples' ./internal/ledger/

// verifySplit fails the test unless got is the correct split. For a given
// input exactly one result satisfies all four rules:
//
//  1. Everyone gets their exact share rounded down, or one unit more.
//  2. The portions add up to the total.
//  3. Extra units go to the largest remainders first.
//  4. Equal remainders go in user-ID order, starting from the person at
//     sorted position rotation mod n and wrapping around.
//
// Exact splits must instead return the inputs unchanged. The returned
// splitCheck holds the working, for logging.
func verifySplit(t *testing.T, total int64, typ SplitType, shares []Share, rotation int64, got []Portion) splitCheck {
	t.Helper()
	in := slices.SortedFunc(slices.Values(shares), func(a, b Share) int { return cmpInt(a.UserID, b.UserID) })
	if len(got) != len(in) {
		t.Fatalf("got %d portions for %d people", len(got), len(in))
	}
	var sum int64
	for i, p := range got {
		if p.UserID != in[i].UserID {
			t.Fatalf("portion %d is for user %d, want user %d (sorted by ID)", i, p.UserID, in[i].UserID)
		}
		sum += p.Amount
	}
	if sum != total {
		t.Fatalf("portions sum to %d, want %d", sum, total)
	}
	c := splitCheck{total: total, typ: typ, rotation: rotation, rows: make([]checkRow, len(in))}
	for i, p := range got {
		c.rows[i] = checkRow{user: p.UserID, got: p.Amount}
	}
	if typ == SplitExact {
		for i, p := range got {
			if p.Amount != in[i].Input {
				t.Fatalf("exact split changed user %d from %d to %d", p.UserID, in[i].Input, p.Amount)
			}
		}
		return c
	}

	n := int64(len(in))
	c.sumW = new(big.Int)
	for i, s := range in {
		w := big.NewInt(s.Input)
		if typ == SplitEqual {
			w = big.NewInt(1)
		}
		c.rows[i].weight = w
		c.sumW.Add(c.sumW, w)
	}
	// big.Int.Mod is Euclidean, so start is in [0, n) even for negative
	// rotations.
	c.start = new(big.Int).Mod(big.NewInt(rotation), big.NewInt(n)).Int64()
	c.leftover = total
	for i := range c.rows {
		r := &c.rows[i]
		num := new(big.Int).Mul(big.NewInt(total), r.weight)
		r.floor, r.rem = new(big.Int).QuoRem(num, c.sumW, new(big.Int))
		r.turn = (int64(i) - c.start + n) % n
		d := new(big.Int).Sub(big.NewInt(r.got), r.floor)
		if !d.IsInt64() || (d.Int64() != 0 && d.Int64() != 1) {
			t.Fatalf("user %d got %d; exact share is %v/%v, so want %v or %v",
				r.user, r.got, num, c.sumW, r.floor, new(big.Int).Add(r.floor, big.NewInt(1)))
		}
		r.extra = d.Int64() == 1
		c.leftover -= r.floor.Int64() // floor ≤ total, so it fits
	}

	for _, a := range c.rows {
		for _, b := range c.rows {
			if !a.extra || b.extra {
				continue
			}
			switch cmp := a.rem.Cmp(b.rem); {
			case cmp < 0:
				t.Fatalf("user %d got an extra unit with remainder %v, but user %d with larger remainder %v didn't",
					a.user, a.rem, b.user, b.rem)
			case cmp == 0 && a.turn > b.turn:
				t.Fatalf("users %d and %d tie on remainder %v; rotation %d puts user %d first, but user %d got the extra unit",
					a.user, b.user, a.rem, rotation, b.user, a.user)
			case cmp == 0:
				c.tieBroken = true
			}
		}
	}
	return c
}

// splitCheck is what verifySplit worked out about one split.
type splitCheck struct {
	total     int64
	typ       SplitType
	rotation  int64
	rows      []checkRow // in user-ID order
	sumW      *big.Int   // nil for exact splits
	start     int64      // sorted position that has the first turn
	leftover  int64      // units left after rounding everyone down
	tieBroken bool       // the rotation decided who got an extra unit
}

type checkRow struct {
	user       int64
	weight     *big.Int
	floor, rem *big.Int // exact share = floor + rem/sumW
	turn       int64    // 0 = first in line for a tied extra unit
	got        int64
	extra      bool
}

// String lays out the derivation as a table, for example:
//
//	equal split of 10000 between 3 people, rotation 0
//	user  weight  exact share  floor  remainder  turn  gets
//	   1       1     3333.333   3333        1/3     0  3334  +1
//	   2       1     3333.333   3333        1/3     1  3333
//	   3       1     3333.333   3333        1/3     2  3333
//	floors sum to 9999, leaving 1: largest remainders first, ties by turn (rotation 0 mod 3 = 0)
//	✓ sums to 10000; everyone within one unit of their exact share; extras follow the rule
func (c splitCheck) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s split of %d between %d people, rotation %d\n", c.typ, c.total, len(c.rows), c.rotation)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	if c.typ == SplitExact {
		fmt.Fprintln(tw, "user\tgets\t")
		for _, r := range c.rows {
			fmt.Fprintf(tw, "%d\t%d\t\n", r.user, r.got)
		}
		_ = tw.Flush()
		fmt.Fprintf(&b, "✓ exact amounts used as given; they sum to %d", c.total)
		return b.String()
	}

	fmt.Fprintln(tw, "user\tweight\texact share\tfloor\tremainder\tturn\tgets\t")
	for _, r := range c.rows {
		exact := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(c.total), r.weight), c.sumW)
		mark := ""
		if r.extra {
			mark = "  +1"
		}
		fmt.Fprintf(tw, "%d\t%v\t%s\t%v\t%v/%v\t%d\t%d\t%s\n",
			r.user, r.weight, exact.FloatString(3), r.floor, r.rem, c.sumW, r.turn, r.got, mark)
	}
	_ = tw.Flush()
	if c.leftover == 0 {
		b.WriteString("floors sum to the total: nothing left over\n")
	} else {
		fmt.Fprintf(&b, "floors sum to %d, leaving %d: largest remainders first", c.total-c.leftover, c.leftover)
		if c.tieBroken {
			fmt.Fprintf(&b, ", ties by turn (rotation %d mod %d = %d)", c.rotation, len(c.rows), c.start)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "✓ sums to %d; everyone within one unit of their exact share; extras follow the rule", c.total)
	return b.String()
}

// split runs Split, verifies the result, logs the working and returns the
// amounts in user-ID order.
func split(t *testing.T, total int64, typ SplitType, shares []Share, rotation int64) []int64 {
	t.Helper()
	out, c := checkedSplit(t, total, typ, shares, rotation)
	t.Log(c)
	return out
}

// quietSplit is split without the log, for loops that would flood it.
func quietSplit(t *testing.T, total int64, typ SplitType, shares []Share, rotation int64) []int64 {
	t.Helper()
	out, _ := checkedSplit(t, total, typ, shares, rotation)
	return out
}

func checkedSplit(t *testing.T, total int64, typ SplitType, shares []Share, rotation int64) ([]int64, splitCheck) {
	t.Helper()
	got, err := Split(total, typ, shares, rotation)
	if err != nil {
		t.Fatalf("Split(%d, %s, %v, %d): %v", total, typ, shares, rotation, err)
	}
	c := verifySplit(t, total, typ, shares, rotation, got)
	out := make([]int64, len(got))
	for i, p := range got {
		out[i] = p.Amount
	}
	return out, c
}

func TestSplitWorkedExamples(t *testing.T) {
	// Amounts are minor units (cents unless noted). Users are 1, 2, 3, …
	// and want lists amounts in user order. Percentages are basis points
	// (1250 = 12.5%).
	tests := []struct {
		name     string
		total    int64
		typ      SplitType
		shares   []Share
		rotation int64
		want     []int64
	}{
		// 9000 / 3 = 3000 exactly.
		{"$90.00 three ways", 9000, SplitEqual, users(1, 2, 3), 0, []int64{3000, 3000, 3000}},
		// 123456 / 4 = 30864 exactly.
		{"$1,234.56 four ways", 123456, SplitEqual, users(1, 2, 3, 4), 0, []int64{30864, 30864, 30864, 30864}},
		// 10000 / 3 = 3333 r 1 each. Floors sum to 9999, so 1 cent is left;
		// remainders tie and rotation 0 gives it to user 1.
		{"$100.00 three ways", 10000, SplitEqual, users(1, 2, 3), 0, []int64{3334, 3333, 3333}},
		// 1000 / 6 = 166 r 4 each. Floors sum to 996, so 4 cents are left
		// and go to users 1–4.
		{"$10.00 six ways", 1000, SplitEqual, users(1, 2, 3, 4, 5, 6), 0, []int64{167, 167, 167, 167, 166, 166}},
		// 1 / 3 = 0 r 1 each. The single cent goes to sorted position 2.
		{"1 cent three ways, rotation 2", 1, SplitEqual, users(1, 2, 3), 2, []int64{0, 0, 1}},
		// JPY has no minor unit: 10000 / 3 = 3333 r 1 → ¥3,334 / ¥3,333 / ¥3,333.
		{"¥10,000 three ways", 10000, SplitEqual, users(1, 2, 3), 0, []int64{3334, 3333, 3333}},
		// KWD has 3 decimals: 1.000 = 1000 fils; 1000 / 3 = 333 r 1, and
		// rotation 1 gives the extra fils to user 2.
		{"KWD 1.000 three ways, rotation 1", 1000, SplitEqual, users(1, 2, 3), 1, []int64{333, 334, 333}},

		// 8000 × 12.5% = 1000 and 8000 × 87.5% = 7000, both exact.
		{"12.5% / 87.5% of $80.00", 8000, SplitPercent, []Share{{1, 1250}, {2, 8750}}, 0, []int64{1000, 7000}},
		// 10000 × 33.33% = 3333 and 10000 × 33.34% = 3334, all exact.
		{"33.33 / 33.33 / 33.34% of $100.00", 10000, SplitPercent, []Share{{1, 3333}, {2, 3333}, {3, 3334}}, 0, []int64{3333, 3333, 3334}},
		// 3333 × 1500 / 10000 = 499.95 → 499 r 9500
		// 3333 × 8500 / 10000 = 2833.05 → 2833 r 500
		// Floors sum to 3332; the 1 cent left goes to the larger remainder.
		{"15% / 85% of $33.33", 3333, SplitPercent, []Share{{1, 1500}, {2, 8500}}, 0, []int64{500, 2833}},
		// 1999 × 60% = 1199.40 → 1199 r 4000
		// 1999 × 25% =  499.75 →  499 r 7500
		// 1999 × 15% =  299.85 →  299 r 8500
		// Floors sum to 1997; 2 cents go to the two largest remainders
		// (users 3 and 2). The biggest share doesn't get a rounding cent.
		{"60 / 25 / 15% of $19.99", 1999, SplitPercent, []Share{{1, 6000}, {2, 2500}, {3, 1500}}, 0, []int64{1199, 500, 300}},
		// 1 × 50% = 0.5 → 0 r 5000 each; the tie goes to user 1.
		{"50 / 50% of 1 cent", 1, SplitPercent, []Share{{1, 5000}, {2, 5000}}, 0, []int64{1, 0}},
		// A 0% participant owes nothing and never gets a rounding cent.
		{"0 / 50 / 50% of $1.01", 101, SplitPercent, []Share{{1, 0}, {2, 5000}, {3, 5000}}, 2, []int64{0, 50, 51}},

		// 25000 × 2/5 = 10000 and 25000 × 1/5 = 5000, all exact.
		{"shares 2:2:1 of $250.00", 25000, SplitShares, []Share{{1, 2}, {2, 2}, {3, 1}}, 0, []int64{10000, 10000, 5000}},
		// 1001 × 2/4 = 500.50 → 500 r 2
		// 1001 × 1/4 = 250.25 → 250 r 1 (twice)
		// Floors sum to 1000; the 1 cent left goes to user 1.
		{"shares 2:1:1 of $10.01", 1001, SplitShares, []Share{{1, 2}, {2, 1}, {3, 1}}, 0, []int64{501, 250, 250}},
		// 7 × 3/5 = 4.2 → 4 r 1
		// 7 × 1/5 = 1.4 → 1 r 2 (twice)
		// Floors sum to 6; users 2 and 3 tie for the 1 cent left, and user 2
		// comes first. User 1 has the biggest share but the smallest remainder.
		{"shares 3:1:1 of 7 cents", 7, SplitShares, []Share{{1, 3}, {2, 1}, {3, 1}}, 0, []int64{4, 2, 1}},

		// Exact amounts are used as given; rotation plays no part.
		{"exact $45 / $30 / $25 of $100.00", 10000, SplitExact, []Share{{1, 4500}, {2, 3000}, {3, 2500}}, 99, []int64{4500, 3000, 2500}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := split(t, tt.total, tt.typ, tt.shares, tt.rotation); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTripWorkedExample follows a small trip from splits to balances to
// settle-up, with every number derived in the comments.
func TestTripWorkedExample(t *testing.T) {
	const mike, chris, ryan = 1, 2, 3
	// Expense 1: Mike pays the $600.01 hotel, split equally.
	// 60001 / 3 = 20000 r 1; rotation 1 gives the cent to Chris.
	hotel := split(t, 60001, SplitEqual, users(mike, chris, ryan), 1)
	// Expense 2: Chris pays $150.00 dinner, 50 / 30 / 20%: 7500, 4500, 3000.
	dinner := split(t, 15000, SplitPercent, []Share{{mike, 5000}, {chris, 3000}, {ryan, 2000}}, 2)
	// Expense 3: Ryan pays $99.99 of show tickets; Mike takes 2 of the 4.
	// 9999 × 2/4 = 4999.50 → 4999 r 2; 9999 × 1/4 = 2499.75 → 2499 r 3.
	// Floors sum to 9997; the 2 cents go to Chris and Ryan (remainder 3).
	show := split(t, 9999, SplitShares, []Share{{mike, 2}, {chris, 1}, {ryan, 1}}, 3)

	for name, c := range map[string]struct{ got, want []int64 }{
		"hotel":  {hotel, []int64{20000, 20001, 20000}},
		"dinner": {dinner, []int64{7500, 4500, 3000}},
		"show":   {show, []int64{4999, 2500, 2500}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Fatalf("%s split = %v, want %v", name, c.got, c.want)
		}
	}

	portions := func(a []int64) []Portion {
		return []Portion{{mike, a[0]}, {chris, a[1]}, {ryan, a[2]}}
	}
	expenses := []Expense{
		{PayerID: mike, Amount: 60001, Portions: portions(hotel)},
		{PayerID: chris, Amount: 15000, Portions: portions(dinner)},
		{PayerID: ryan, Amount: 9999, Portions: portions(show)},
	}
	// Mike:  paid 60001, owes 20000 + 7500 + 4999 = 32499 → +27502
	// Chris: paid 15000, owes 20001 + 4500 + 2500 = 27001 → −12001
	// Ryan:  paid  9999, owes 20000 + 3000 + 2500 = 25500 → −15501
	// Check: 27502 − 12001 − 15501 = 0.
	bal := Balances(expenses, nil)
	if want := map[int64]int64{mike: 27502, chris: -12001, ryan: -15501}; !mapsEqual(bal, want) {
		t.Fatalf("balances = %v, want %v", bal, want)
	}
	t.Logf("balances: Mike %+d, Chris %+d, Ryan %+d (sum %d)", bal[mike], bal[chris], bal[ryan], bal[mike]+bal[chris]+bal[ryan])
	// The biggest debtor (Ryan) pays first, then Chris clears the rest.
	pays := SettleUp(bal)
	if want := []Payment{{ryan, mike, 15501}, {chris, mike, 12001}}; !slices.Equal(pays, want) {
		t.Fatalf("settle up = %v, want %v", pays, want)
	}
	var settled []Settlement
	for _, p := range pays {
		t.Logf("settle up: user %d pays user %d %d", p.From, p.To, p.Amount)
		settled = append(settled, Settlement(p))
	}
	t.Log("✓ every balance is zero after the suggested payments")
	for id, v := range Balances(expenses, settled) {
		if v != 0 {
			t.Fatalf("user %d still has %d after settling up", id, v)
		}
	}
}

// randomParts cuts total into n non-negative parts.
func randomParts(rng *rand.Rand, total int64, n int) []int64 {
	cuts := make([]int64, n-1)
	for i := range cuts {
		cuts[i] = rng.Int64N(total + 1)
	}
	slices.Sort(cuts)
	out := make([]int64, n)
	prev := int64(0)
	for i, c := range cuts {
		out[i] = c - prev
		prev = c
	}
	out[n-1] = total - prev
	return out
}

// randomSplit returns a valid Split input. User IDs are scattered and
// unsorted, totals range from fewer cents than people up to MaxAmount, and
// rotations cover all of int64 including negatives.
func randomSplit(rng *rand.Rand) (int64, SplitType, []Share, int64) {
	n := 1 + rng.IntN(15)
	var total int64
	switch rng.IntN(3) {
	case 0:
		total = 1 + rng.Int64N(int64(3*n))
	case 1:
		total = 1 + rng.Int64N(100_000)
	default:
		total = 1 + rng.Int64N(MaxAmount)
	}
	shares := make([]Share, n)
	for i, id := range rng.Perm(1000)[:n] {
		shares[i].UserID = int64(id + 1)
	}
	typ := SplitTypes[rng.IntN(len(SplitTypes))]
	switch typ {
	case SplitExact:
		for i, v := range randomParts(rng, total, n) {
			shares[i].Input = v
		}
	case SplitPercent:
		for i, v := range randomParts(rng, FullPercent, n) {
			shares[i].Input = v
		}
	case SplitShares:
		maxW := MaxShareWeight
		if rng.IntN(2) == 0 {
			maxW = 4 // small weights make remainder ties common
		}
		for i := range shares {
			shares[i].Input = 1 + rng.Int64N(maxW)
		}
	}
	return total, typ, shares, int64(rng.Uint64()) //nolint:gosec // any bit pattern is a valid rotation
}

func TestSplitMatchesLargestRemainderRule(t *testing.T) {
	const cases = 20_000
	rng := rand.New(rand.NewPCG(5, 6))
	byType := map[SplitType]int{}
	var withLeftover, leftoverUnits, tieBroken, maxPeople int
	var maxTotal int64
	var samples []splitCheck
	for range cases {
		total, typ, shares, rot := randomSplit(rng)
		before := slices.Clone(shares)
		got, err := Split(total, typ, shares, rot)
		if err != nil {
			t.Fatalf("Split(%d, %s, %v, %d): %v", total, typ, shares, rot, err)
		}
		c := verifySplit(t, total, typ, shares, rot, got)
		if !slices.Equal(shares, before) {
			t.Fatalf("Split modified its input: %v → %v", before, shares)
		}

		byType[typ]++
		if c.leftover > 0 {
			withLeftover++
			leftoverUnits += int(c.leftover)
		}
		if c.tieBroken {
			tieBroken++
			if len(samples) < 2 && len(c.rows) <= 6 {
				samples = append(samples, c)
			}
		}
		maxPeople, maxTotal = max(maxPeople, len(shares)), max(maxTotal, total)
	}
	t.Logf("✓ %d random splits verified: %d equal, %d exact, %d percent, %d shares",
		cases, byType[SplitEqual], byType[SplitExact], byType[SplitPercent], byType[SplitShares])
	t.Logf("  %d had units left over after rounding down (%d units in all); in %d the rotation broke a tie",
		withLeftover, leftoverUnits, tieBroken)
	t.Logf("  up to %d people, totals up to %d", maxPeople, maxTotal)
	for _, c := range samples {
		t.Logf("sample where the rotation decided:\n%v", c)
	}
}

func TestSplitAtInt64Limits(t *testing.T) {
	// Split computes total × weight in int64. These caps keep that product
	// in range; raising either one needs a wider type in largestRemainder.
	if MaxAmount > math.MaxInt64/MaxShareWeight {
		t.Fatalf("MaxAmount × MaxShareWeight overflows int64")
	}
	if FullPercent > MaxShareWeight {
		t.Fatalf("FullPercent exceeds MaxShareWeight")
	}

	many := make([]Share, 15)
	for i := range many {
		many[i] = Share{UserID: int64(i + 1), Input: MaxShareWeight - int64(i)}
	}
	tests := []struct {
		name   string
		total  int64
		typ    SplitType
		shares []Share
	}{
		{"max weights", MaxAmount, SplitShares, []Share{{1, MaxShareWeight}, {2, MaxShareWeight}}},
		{"lopsided weights", MaxAmount, SplitShares, []Share{{1, MaxShareWeight}, {2, MaxShareWeight - 1}, {3, 1}}},
		{"15 near-max weights", MaxAmount - 1, SplitShares, many},
		{"99.99 / 0.01%", MaxAmount, SplitPercent, []Share{{1, 9999}, {2, 1}}},
		{"equal seven ways", MaxAmount - 1, SplitEqual, users(1, 2, 3, 4, 5, 6, 7)},
		{"exact max", MaxAmount, SplitExact, []Share{{1, MaxAmount}, {2, 0}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			split(t, tt.total, tt.typ, tt.shares, 0)
			for _, rot := range []int64{1, -1, math.MaxInt64, math.MinInt64} {
				quietSplit(t, tt.total, tt.typ, tt.shares, rot)
			}
			t.Log("✓ also verified at rotations 1, −1, MaxInt64 and MinInt64")
		})
	}
}

// TestRoundRobinOneLeftoverVisitsEveryone checks that with one leftover
// unit and n people, rotation r gives it to the person at sorted position
// r mod n. Positions follow sorted user ID, not input order or ID value.
func TestRoundRobinOneLeftoverVisitsEveryone(t *testing.T) {
	ids := []int64{42, 7, 1000, 3, 99, 15, 8, 500, 61, 2}
	for n := 1; n <= len(ids); n++ {
		sorted := slices.Sorted(slices.Values(ids[:n]))
		total := int64(100*n + 1)
		var winners []int64
		for rot := int64(-2 * n); rot < int64(3*n); rot++ {
			pos := rot
			for pos < 0 {
				pos += int64(n)
			}
			winner := sorted[pos%int64(n)]
			winners = append(winners, winner)
			got, err := Split(total, SplitEqual, users(ids[:n]...), rot)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range got {
				want := int64(100)
				if p.UserID == winner {
					want = 101
				}
				if p.Amount != want {
					t.Fatalf("n=%d rotation=%d: user %d got %d, want %d (extra unit belongs to user %d)",
						n, rot, p.UserID, p.Amount, want, winner)
				}
			}
		}
		t.Logf("✓ %d people %v, rotations %d..%d: extra unit went to %v", n, sorted, -2*n, 3*n-1, winners)
	}
}

// TestRoundRobinWindowWraps checks that k leftover units go to k
// consecutive people starting at the rotation, wrapping past the last
// person back to the first.
func TestRoundRobinWindowWraps(t *testing.T) {
	five := users(10, 20, 30, 40, 50)
	tests := []struct {
		rotation int64
		leftover int64
		want     []int64 // users who get an extra unit
	}{
		{0, 0, nil},
		{0, 1, []int64{10}},
		{0, 3, []int64{10, 20, 30}},
		{3, 2, []int64{40, 50}},
		{4, 2, []int64{10, 50}},         // 50, then wraps to 10
		{4, 4, []int64{10, 20, 30, 50}}, // 50, 10, 20, 30
		{7, 3, []int64{30, 40, 50}},     // 7 mod 5 = 2 → starts at 30
		{-1, 1, []int64{50}},            // −1 mod 5 = 4 → starts at 50
		{-6, 2, []int64{10, 50}},        // −6 mod 5 = 4
	}
	for _, tt := range tests {
		got, err := Split(500+tt.leftover, SplitEqual, five, tt.rotation)
		if err != nil {
			t.Fatal(err)
		}
		var extra []int64
		for _, p := range got {
			if p.Amount == 101 {
				extra = append(extra, p.UserID)
			}
		}
		if !slices.Equal(extra, tt.want) {
			t.Errorf("rotation %d, %d left over: extra units went to %v, want %v", tt.rotation, tt.leftover, extra, tt.want)
		}
		t.Logf("rotation %2d, %d left over → extra units to %v", tt.rotation, tt.leftover, extra)
	}

	// Every rotation and leftover count, against the rule written out. The
	// log draws each case as a row of users 10–50, ■ marking an extra unit.
	for k := range int64(5) {
		for r := range int64(10) {
			got := quietSplit(t, 500+k, SplitEqual, five, r)
			var row strings.Builder
			for i, amt := range got {
				inWindow := (int64(i)-r%5+5)%5 < k
				if (amt == 101) != inWindow {
					t.Fatalf("rotation %d, %d left over: position %d got %d", r, k, i, amt)
				}
				if amt == 101 {
					row.WriteString(" ■")
				} else {
					row.WriteString(" ·")
				}
			}
			if r < 5 {
				t.Logf("%d left over, rotation %d:%s", k, r, row.String())
			}
		}
	}
	t.Log("✓ rotations 5–9 also follow the rule (they wrap round to 0–4)")
}

// TestRoundRobinEvensOutAcrossExpenses checks the point of rotating: over
// n consecutive expense IDs with k leftover units each, every person picks
// up exactly k extra units.
func TestRoundRobinEvensOutAcrossExpenses(t *testing.T) {
	for n := int64(2); n <= 8; n++ {
		for k := int64(1); k < n; k++ {
			extra := map[int64]int64{}
			const rounds = 4
			for expenseID := int64(1); expenseID <= rounds*n; expenseID++ {
				got := quietSplit(t, 100*n+k, SplitEqual, users(int64Range(n)...), expenseID)
				for i, amt := range got {
					extra[int64(i+1)] += amt - 100
				}
			}
			for id := int64(1); id <= n; id++ {
				if extra[id] != rounds*k {
					t.Fatalf("n=%d k=%d: extra units per user = %v, want %d each", n, k, extra, rounds*k)
				}
			}
			t.Logf("✓ %d people, %d left over per expense, expenses 1–%d: everyone got %d extra units",
				n, k, rounds*n, rounds*k)
		}
	}
}

// TestRoundRobinOnlyBreaksTies checks that rotation never overrides a
// larger remainder; it only orders people whose remainders are equal.
func TestRoundRobinOnlyBreaksTies(t *testing.T) {
	tests := []struct {
		name   string
		total  int64
		typ    SplitType
		shares []Share
		want   [][]int64 // result for rotation 0, 1, 2, …
	}{
		// 10 × 2/3 = 6.67 → 6 r 2 and 10 × 1/3 = 3.33 → 3 r 1. User 1's
		// larger remainder wins whatever the rotation.
		{"shares 2:1 of 10", 10, SplitShares, []Share{{1, 2}, {2, 1}},
			[][]int64{{7, 3}, {7, 3}, {7, 3}, {7, 3}}},
		// Remainders 9500 vs 500 (see TestSplitWorkedExamples).
		{"15 / 85% of $33.33", 3333, SplitPercent, []Share{{1, 1500}, {2, 8500}},
			[][]int64{{500, 2833}, {500, 2833}}},
		// Remainders 4000, 7500, 8500: users 2 and 3 always get the cents.
		{"60 / 25 / 15% of $19.99", 1999, SplitPercent, []Share{{1, 6000}, {2, 2500}, {3, 1500}},
			[][]int64{{1199, 500, 300}, {1199, 500, 300}, {1199, 500, 300}}},
		// Remainders 1, 2, 2: only users 2 and 3 tie. Rotation 0 starts at
		// user 1, who isn't in the tie, so the next in line (user 2) gets
		// it; rotation 1 starts at user 2; rotation 2 starts at user 3.
		// User 1 never gets the cent.
		{"shares 3:1:1 of 7", 7, SplitShares, []Share{{1, 3}, {2, 1}, {3, 1}},
			[][]int64{{4, 2, 1}, {4, 2, 1}, {4, 1, 2}, {4, 2, 1}}},
		// An exact 50/50 tie on an odd amount alternates.
		{"50 / 50% of $1.01", 101, SplitPercent, []Share{{1, 5000}, {2, 5000}},
			[][]int64{{51, 50}, {50, 51}, {51, 50}, {50, 51}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for rot, want := range tt.want {
				if got := split(t, tt.total, tt.typ, tt.shares, int64(rot)); !slices.Equal(got, want) {
					t.Errorf("rotation %d: got %v, want %v", rot, got, want)
				}
			}
		})
	}
}

// TestRoundRobinExtremeRotations checks that rotation is taken mod n
// correctly for any int64, including values where naive arithmetic would
// overflow.
func TestRoundRobinExtremeRotations(t *testing.T) {
	tests := []struct {
		rotation int64
		want     []int64
	}{
		{0, []int64{34, 33, 33}},
		{-3, []int64{34, 33, 33}},                        // −3 mod 3 = 0
		{3_000_000_000_000_000_000, []int64{34, 33, 33}}, // divisible by 3
		{math.MaxInt64, []int64{33, 34, 33}},             // 2⁶³ − 1 ≡ 1 (mod 3)
		{math.MinInt64, []int64{33, 34, 33}},             // −2⁶³ ≡ 1 (mod 3)
		{math.MinInt64 + 1, []int64{33, 33, 34}},         // −(2⁶³ − 1) ≡ 2 (mod 3)
	}
	for _, tt := range tests {
		if got := split(t, 100, SplitEqual, users(1, 2, 3), tt.rotation); !slices.Equal(got, tt.want) {
			t.Errorf("rotation %d: got %v, want %v", tt.rotation, got, tt.want)
		}
	}
}

func TestRoundRobinIgnoresExactSplits(t *testing.T) {
	shares := []Share{{1, 4500}, {2, 3000}, {3, 2500}}
	rots := []int64{0, 1, 2, -1, math.MaxInt64, math.MinInt64}
	for _, rot := range rots {
		if got := quietSplit(t, 10000, SplitExact, shares, rot); !slices.Equal(got, []int64{4500, 3000, 2500}) {
			t.Errorf("rotation %d changed an exact split: %v", rot, got)
		}
	}
	t.Logf("✓ exact split 4500 / 3000 / 2500 unchanged at rotations %v", rots)
}

// TestRoundRobinIgnoresInputOrder checks that the order participants are
// listed in has no effect: the tiebreak uses sorted user IDs.
func TestRoundRobinIgnoresInputOrder(t *testing.T) {
	const cases = 2000
	rng := rand.New(rand.NewPCG(7, 8))
	for range cases {
		total, typ, shares, rot := randomSplit(rng)
		want, err := Split(total, typ, shares, rot)
		if err != nil {
			t.Fatal(err)
		}
		shuffled := slices.Clone(shares)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got, err := Split(total, typ, shuffled, rot)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("reordering %v as %v changed the split from %v to %v", shares, shuffled, want, got)
		}
	}
	t.Logf("✓ %d random splits gave the same result with participants shuffled", cases)
}
