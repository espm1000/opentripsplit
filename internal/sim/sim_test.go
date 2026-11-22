package sim

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/espm1000/opentripsplit/internal/ledger"
)

// failures returns every failed check in the trip, with its step.
func failures(t Trip) []string {
	var out []string
	for _, e := range t.Events {
		for _, c := range e.Failed() {
			out = append(out, fmt.Sprintf("step %d %s: %s", e.N, c.Name, c.Detail))
		}
	}
	return out
}

func checkTrip(t *testing.T, p Params) Trip {
	t.Helper()
	trip := Run(p)
	if f := failures(trip); len(f) > 0 {
		t.Fatalf("seed %d, %d people, %d expenses (%s): %v", p.Seed, p.People, p.Expenses, trip.Currency.Code, f)
	}
	return trip
}

// TestManyTrips is a property test: every invariant holds on thousands of
// random trips, across all currencies and group sizes.
func TestManyTrips(t *testing.T) {
	n := 2000
	if testing.Short() {
		n = 200
	}
	splitTypes := map[ledger.SplitType]int{}
	decimals := map[int]bool{}
	for seed := range uint64(n) {
		p := Params{Seed: seed, People: MinPeople + int(seed)%(MaxPeople-MinPeople+1), Expenses: MinExpenses + int(seed*7)%(MaxExpenses-MinExpenses+1)}
		trip := checkTrip(t, p)
		decimals[trip.Currency.Decimals] = true
		for _, e := range trip.Events {
			if e.Expense != nil {
				splitTypes[e.Expense.SplitType]++
			}
		}
	}
	for _, st := range ledger.SplitTypes {
		if splitTypes[st] == 0 {
			t.Errorf("no %s splits simulated", st)
		}
	}
	for _, d := range []int{0, 2, 3} {
		if !decimals[d] {
			t.Errorf("no currency with %d decimals simulated", d)
		}
	}
}

func FuzzRun(f *testing.F) {
	f.Add(uint64(0), 4, 12, uint8(255))
	f.Add(uint64(42), 8, 40, uint8(0))
	f.Add(uint64(1<<63), 2, 5, uint8(18))
	f.Fuzz(func(t *testing.T, seed uint64, people, expenses int, currency uint8) {
		p := Params{Seed: seed, People: people, Expenses: expenses}
		if int(currency) < len(ledger.Currencies) { // otherwise the seed picks
			p.Currency = ledger.Currencies[currency].Code
		}
		checkTrip(t, p)
	})
}

func TestDeterministic(t *testing.T) {
	p := Params{Seed: 7, People: 5, Expenses: 20}
	if a, b := Run(p), Run(p); !reflect.DeepEqual(a, b) {
		t.Fatal("same params gave different trips")
	}
	if a, b := Run(p), Run(Params{Seed: 8, People: 5, Expenses: 20}); reflect.DeepEqual(a.Events, b.Events) {
		t.Fatal("different seeds gave the same trip")
	}
}

func TestCurrency(t *testing.T) {
	for _, c := range ledger.Currencies {
		trip := checkTrip(t, Params{Seed: 11, People: 6, Expenses: 15, Currency: c.Code})
		if trip.Currency != c {
			t.Fatalf("asked for %s, got %s", c.Code, trip.Currency.Code)
		}
	}
	a, b := Run(Params{Seed: 11, Currency: "JPY"}), Run(Params{Seed: 11, Currency: "KWD"})
	if !reflect.DeepEqual(a.People, b.People) {
		t.Error("changing only the currency changed the people")
	}
	if trip := Run(Params{Seed: 11, Currency: "XXX"}); trip.Params.Currency != "" {
		t.Errorf("unknown currency kept: %q", trip.Params.Currency)
	}
}

func TestShape(t *testing.T) {
	trip := checkTrip(t, Params{Seed: 3, People: 99, Expenses: -1})
	if trip.Params.People != MaxPeople || len(trip.People) != MaxPeople || trip.Params.Expenses != MinExpenses {
		t.Fatalf("params not clamped: %+v", trip.Params)
	}
	var expenses int
	settling := false
	day := 1
	for i, e := range trip.Events {
		if e.N != i+1 {
			t.Fatalf("event %d numbered %d", i, e.N)
		}
		if e.Day < day || e.Day > trip.Days {
			t.Fatalf("event %d on day %d, after day %d of %d", e.N, e.Day, day, trip.Days)
		}
		day = e.Day
		if e.Kind == KindSettle && e.Day != trip.Days {
			t.Fatalf("settle-up on day %d, not the last day %d", e.Day, trip.Days)
		}
		switch e.Kind {
		case KindExpense:
			expenses++
			if settling {
				t.Fatal("expense after settle-up started")
			}
		case KindSettle:
			settling = true
		}
	}
	if expenses != MinExpenses {
		t.Fatalf("%d expenses, want %d", expenses, MinExpenses)
	}
	last := trip.Events[len(trip.Events)-1]
	if len(last.Suggested) != 0 || last.Checks[len(last.Checks)-1].Name != "All square" {
		t.Fatalf("trip doesn't end settled: %+v", last)
	}
}

// TestChecksCatchBadMath makes sure the checks can fail: a broken split
// and a broken settle-up are reported.
func TestChecksCatchBadMath(t *testing.T) {
	r := &runner{trip: &Trip{Currency: ledger.Currency{Code: "USD", Decimals: 2}, People: []Person{{1, "A"}, {2, "B"}}}, running: map[int64]int64{}}
	e := &Expense{Amount: 100, SplitType: ledger.SplitEqual,
		Shares:   []ledger.Share{{UserID: 1}, {UserID: 2}},
		Portions: []ledger.Portion{{UserID: 1, Amount: 51}, {UserID: 2, Amount: 48}}}
	if c := r.splitAddsUp(e); c.OK {
		t.Error("split that doesn't add up passed")
	}
	if c := r.fairRounding(e); c.OK {
		t.Error("unfair rounding passed")
	}
	b := map[int64]int64{1: 100, 2: -100}
	if c := r.settleUpWorks(b, []ledger.Payment{{From: 2, To: 1, Amount: 60}}); c.OK {
		t.Error("settle-up that leaves a balance passed")
	}
	if c := r.settleUpWorks(b, []ledger.Payment{{From: 1, To: 2, Amount: 100}}); c.OK {
		t.Error("settle-up paying the wrong way passed")
	}
	if c := r.sumZero(map[int64]int64{1: 1}); c.OK {
		t.Error("non-zero sum passed")
	}
}
