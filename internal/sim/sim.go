// Package sim plays random trips through the ledger: expenses, partial
// paybacks and a final settle-up, checking the money invariants after every
// step. It is pure (no I/O) and deterministic: the same Params always give
// the same Trip, so a failing trip can be replayed from its seed.
package sim

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/espm1000/opentripsplit/internal/ledger"
)

// Limits for Params. They keep a trip small enough to recompute from
// scratch on every request.
const (
	MinPeople   = 2
	MaxPeople   = 8
	MinExpenses = 5
	MaxExpenses = 40
)

// Params picks a trip.
type Params struct {
	Seed     uint64
	People   int
	Expenses int
	// Currency is an ISO code from ledger.Currencies. Empty (or unknown)
	// lets the seed pick one.
	Currency string
}

// Clamp brings People and Expenses within limits and clears an unknown
// Currency.
func (p Params) Clamp() Params {
	p.People = min(max(p.People, MinPeople), MaxPeople)
	p.Expenses = min(max(p.Expenses, MinExpenses), MaxExpenses)
	if _, ok := ledger.LookupCurrency(p.Currency); !ok {
		p.Currency = ""
	}
	return p
}

// Person is a simulated traveler. IDs run from 1 to People.
type Person struct {
	ID   int64
	Name string
}

// EventKind says what happened at a step.
type EventKind string

const (
	KindExpense EventKind = "expense" // someone paid for something
	KindPayment EventKind = "payment" // someone paid part of their debt back mid-trip
	KindSettle  EventKind = "settle"  // one suggested settle-up payment, at the end
)

// Expense is a simulated expense and how the ledger split it.
type Expense struct {
	ID          int64 // 1-based; also the Split rotation, as the store uses the expense ID
	PayerID     int64
	Description string
	Amount      int64
	SplitType   ledger.SplitType
	Shares      []ledger.Share
	Portions    []ledger.Portion
}

// Event is one step of the trip and the state after it.
type Event struct {
	N         int // 1-based step number
	Day       int // trip day, from 1; settle-up happens on the last day
	Kind      EventKind
	Expense   *Expense        // KindExpense
	Payment   *ledger.Payment // KindPayment, KindSettle
	Balances  map[int64]int64 // every person, zero included
	Changed   map[int64]int64 // balance change at this step; zero changes left out
	Suggested []ledger.Payment
	Checks    []Check
}

// Check is one invariant tested at a step.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Failed returns the checks that did not hold.
func (e Event) Failed() []Check {
	var out []Check
	for _, c := range e.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

// Trip is a whole simulated trip.
type Trip struct {
	Params   Params
	Currency ledger.Currency
	People   []Person
	Events   []Event
	Days     int // about three expenses a day
}

// names are common North American first names. Most are six letters or
// more, so the UI gets tested with names that wrap; short ones are rare on
// purpose.
var names = []string{
	"Michael", "Jennifer", "Christopher", "Jessica", "Matthew", "Ashley",
	"Joshua", "Amanda", "Daniel", "Sarah", "David", "Emily",
	"James", "Megan", "Andrew", "Lauren", "Ryan", "Nicole",
	"Brandon", "Stephanie", "Tyler", "Rachel", "Justin", "Samantha",
	"Kevin", "Elizabeth", "Jacob", "Hannah", "William", "Amy",
}

var descriptions = []string{
	"Taxi from the airport", "Groceries", "Hostel", "Museum tickets", "Ferry",
	"Dinner", "Coffee", "Fuel", "Snacks", "Boat rental", "Ice cream",
	"Train tickets", "Bike hire", "Laundry", "Souvenirs", "Pizza",
}

// Run plays the trip picked by p (after Clamp).
func Run(p Params) Trip {
	p = p.Clamp()
	rng := rand.New(rand.NewPCG(p.Seed, 0x6f74732d73696d)) // #nosec G404 -- a replayable simulation, not security
	// Drawn even when Currency is set, so the people and payers stay the
	// same when only the currency changes.
	t := Trip{Params: p, Currency: ledger.Currencies[rng.IntN(len(ledger.Currencies))]}
	if c, ok := ledger.LookupCurrency(p.Currency); ok {
		t.Currency = c
	}
	for i, n := range rng.Perm(len(names))[:p.People] {
		t.People = append(t.People, Person{ID: int64(i + 1), Name: names[n]})
	}
	r := runner{trip: &t, rng: rng, running: map[int64]int64{}}
	// Days don't use rng, so they don't change which trip a seed plays.
	t.Days = 1 + p.Expenses/3

	for i := range p.Expenses {
		r.day = 1 + i*t.Days/p.Expenses
		r.expense(int64(i + 1))
		// Now and then someone pays part of their debt back.
		if i > 0 && i < p.Expenses-1 && rng.IntN(8) == 0 {
			r.payment()
		}
	}
	// Settle-up needs at most People−1 payments; the bound stops a broken
	// SettleUp from looping forever (the checks then report it).
	r.day = t.Days
	for i := 0; i < p.People && len(r.last().Suggested) > 0; i++ {
		r.settle()
	}
	if len(t.Events) > 0 {
		last := &t.Events[len(t.Events)-1]
		last.Checks = append(last.Checks, r.allSquare(last.Balances))
	}
	return t
}

// runner holds the state while a trip is played.
type runner struct {
	trip        *Trip
	rng         *rand.Rand
	expenses    []ledger.Expense
	settlements []ledger.Settlement
	// running is a balance total kept step by step, separately from
	// ledger.Balances, which recomputes from every expense and settlement.
	running map[int64]int64
	day     int // trip day for the next event
}

func (r *runner) last() Event {
	if n := len(r.trip.Events); n > 0 {
		return r.trip.Events[n-1]
	}
	return Event{}
}

func (r *runner) money(v int64) string {
	return ledger.FormatAmount(v, r.trip.Currency.Decimals) + " " + r.trip.Currency.Code
}

func (r *runner) name(id int64) string {
	return r.trip.People[id-1].Name
}

func (r *runner) expense(id int64) {
	rng, people := r.rng, r.trip.People
	e := &Expense{
		ID:          id,
		PayerID:     people[rng.IntN(len(people))].ID,
		Description: descriptions[rng.IntN(len(descriptions))],
		SplitType:   ledger.SplitTypes[rng.IntN(len(ledger.SplitTypes))],
	}
	var participants []int64
	for _, i := range rng.Perm(len(people))[:1+rng.IntN(len(people))] {
		participants = append(participants, people[i].ID)
	}
	slices.Sort(participants)
	e.Amount = r.amount(len(participants))

	var inputs []int64
	switch e.SplitType {
	case ledger.SplitExact:
		inputs = partition(rng, e.Amount, len(participants), 1)
	case ledger.SplitPercent:
		// Coarse steps (5%, 25%) leave some people at 0%.
		step := []int64{1, 100, 500, 2500}[rng.IntN(4)]
		inputs = partition(rng, ledger.FullPercent, len(participants), step)
	case ledger.SplitShares:
		for range participants {
			inputs = append(inputs, 1+rng.Int64N(5))
		}
	}
	for i, uid := range participants {
		sh := ledger.Share{UserID: uid}
		if inputs != nil {
			sh.Input = inputs[i]
		}
		e.Shares = append(e.Shares, sh)
	}

	ev := Event{Kind: KindExpense, Expense: e}
	portions, err := ledger.Split(e.Amount, e.SplitType, e.Shares, e.ID)
	if err != nil {
		ev.Checks = append(ev.Checks, Check{"Split accepted", false, err.Error()})
	} else {
		e.Portions = portions
		r.expenses = append(r.expenses, ledger.Expense{PayerID: e.PayerID, Amount: e.Amount, Portions: portions})
		r.running[e.PayerID] += e.Amount
		for _, p := range portions {
			r.running[p.UserID] -= p.Amount
		}
		ev.Checks = append(ev.Checks, r.splitAddsUp(e), r.fairRounding(e))
	}
	ev.Checks = append(ev.Checks, r.roundTrip(e.Amount))
	r.push(ev)
}

// minPurchase is the smallest expense, in major units (2.00 USD). It only
// applies to the simulator: smaller purchases look unrealistic in a demo.
const minPurchase = 2

// minAmount is minPurchase in c's minor units. Currencies without minor
// units are scaled up 100 times, like every simulated amount in them.
func minAmount(c ledger.Currency) int64 {
	return minPurchase * majorUnit(c)
}

// majorUnit is how many minor units a simulated "1" is in c: 100 cents,
// 1,000 fils, or 100 yen (see amount).
func majorUnit(c ledger.Currency) int64 {
	if c.Decimals == 0 {
		return 100
	}
	unit := int64(1)
	for range c.Decimals {
		unit *= 10
	}
	return unit
}

// amount picks an expense total in minor units, at least minAmount. About
// one in ten is the minimum plus fewer minor units than there are people,
// so rounding leftovers and tie rotation get exercised.
func (r *runner) amount(n int) int64 {
	c := r.trip.Currency
	if r.rng.IntN(10) == 0 {
		return minAmount(c) + r.rng.Int64N(int64(n))
	}
	// 5.00 to 300.99 USD; 500 to 30,000 yen, krona, ...
	major := 5 + r.rng.Int64N(296)
	cents := int64(0)
	if c.Decimals > 0 {
		cents = r.rng.Int64N(majorUnit(c))
	}
	return major*majorUnit(c) + cents
}

// partition splits total into n non-negative parts that are multiples of
// step (total must be one) and add up to total.
func partition(rng *rand.Rand, total int64, n int, step int64) []int64 {
	units := total / step
	cuts := []int64{0, units}
	for range n - 1 {
		cuts = append(cuts, rng.Int64N(units+1))
	}
	slices.Sort(cuts)
	out := make([]int64, n)
	for i := range out {
		out[i] = (cuts[i+1] - cuts[i]) * step
	}
	rng.Shuffle(n, func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// payment records a partial payback from someone who owes to someone who
// is owed.
func (r *runner) payment() {
	var debtors, creditors []int64
	for _, p := range r.trip.People {
		switch v := r.running[p.ID]; {
		case v < 0:
			debtors = append(debtors, p.ID)
		case v > 0:
			creditors = append(creditors, p.ID)
		}
	}
	if len(debtors) == 0 || len(creditors) == 0 {
		return
	}
	from, to := debtors[r.rng.IntN(len(debtors))], creditors[r.rng.IntN(len(creditors))]
	most := min(-r.running[from], r.running[to])
	r.pay(KindPayment, ledger.Payment{From: from, To: to, Amount: 1 + r.rng.Int64N(most)})
}

// settle makes the first suggested settle-up payment.
func (r *runner) settle() {
	prev := r.last().Suggested
	r.pay(KindSettle, prev[0])
	ev := &r.trip.Events[len(r.trip.Events)-1]
	ok := slices.Equal(ev.Suggested, prev[1:])
	detail := fmt.Sprintf("%d payment(s) left, as planned", len(ev.Suggested))
	if !ok {
		detail = fmt.Sprintf("expected %v after paying %v, got %v", prev[1:], prev[0], ev.Suggested)
	}
	ev.Checks = append(ev.Checks, Check{"Plan holds", ok, detail})
}

func (r *runner) pay(kind EventKind, p ledger.Payment) {
	r.settlements = append(r.settlements, ledger.Settlement(p))
	r.running[p.From] += p.Amount
	r.running[p.To] -= p.Amount
	r.push(Event{Kind: kind, Payment: &p})
}

// push fills in the state after ev and the checks every step gets, then
// appends it.
func (r *runner) push(ev Event) {
	prev := r.last().Balances
	ev.N, ev.Day = len(r.trip.Events)+1, r.day
	ev.Balances, ev.Changed = map[int64]int64{}, map[int64]int64{}
	got := ledger.Balances(r.expenses, r.settlements)
	for _, p := range r.trip.People {
		ev.Balances[p.ID] = got[p.ID]
		if d := got[p.ID] - prev[p.ID]; d != 0 {
			ev.Changed[p.ID] = d
		}
	}
	ev.Suggested = ledger.SettleUp(got)
	ev.Checks = append(ev.Checks, r.balancesMatch(got), r.sumZero(got), r.settleUpWorks(got, ev.Suggested))
	r.trip.Events = append(r.trip.Events, ev)
}

func (r *runner) splitAddsUp(e *Expense) Check {
	var sum int64
	var parts []string
	for _, p := range e.Portions {
		sum += p.Amount
		parts = append(parts, ledger.FormatAmount(p.Amount, r.trip.Currency.Decimals))
	}
	return Check{"Split adds up", sum == e.Amount, strings.Join(parts, " + ") + " = " + r.money(sum) + " of " + r.money(e.Amount)}
}

// fairRounding checks each portion is its exact share rounded down, or
// rounded up by the one minor unit that largest remainder hands out.
func (r *runner) fairRounding(e *Expense) Check {
	c := Check{Name: "Fair rounding", OK: len(e.Portions) == len(e.Shares)}
	if !c.OK {
		c.Detail = fmt.Sprintf("%d people in, %d portions out", len(e.Shares), len(e.Portions))
		return c
	}
	byUser := map[int64]int64{}
	for _, p := range e.Portions {
		byUser[p.UserID] = p.Amount
	}
	if e.SplitType == ledger.SplitExact {
		for _, s := range e.Shares {
			if byUser[s.UserID] != s.Input {
				return Check{c.Name, false, fmt.Sprintf("%s entered %s but got %s", r.name(s.UserID), r.money(s.Input), r.money(byUser[s.UserID]))}
			}
		}
		return Check{c.Name, true, "exact amounts used as entered"}
	}
	var sumW int64
	weight := func(s ledger.Share) int64 {
		if e.SplitType == ledger.SplitEqual {
			return 1
		}
		return s.Input
	}
	for _, s := range e.Shares {
		sumW += weight(s)
	}
	var ups int
	for _, s := range e.Shares {
		floor := e.Amount * weight(s) / sumW
		switch got := byUser[s.UserID]; got {
		case floor:
		case floor + 1:
			ups++
		default:
			return Check{c.Name, false, fmt.Sprintf("%s got %s, exact share rounds down to %s", r.name(s.UserID), r.money(got), r.money(floor))}
		}
	}
	return Check{c.Name, true, fmt.Sprintf("%d of %d rounded up by one unit", ups, len(e.Shares))}
}

func (r *runner) roundTrip(v int64) Check {
	s := ledger.FormatAmount(v, r.trip.Currency.Decimals)
	got, err := ledger.ParseAmount(s, r.trip.Currency.Decimals)
	c := Check{"Amount round-trips", err == nil && got == v, fmt.Sprintf("%d → %q → %d", v, s, got)}
	if err != nil {
		c.Detail = fmt.Sprintf("%d → %q → %v", v, s, err)
	}
	return c
}

func (r *runner) balancesMatch(got map[int64]int64) Check {
	for _, p := range r.trip.People {
		if got[p.ID] != r.running[p.ID] {
			return Check{"Balances match", false, fmt.Sprintf("%s: ledger says %s, running total says %s", p.Name, r.money(got[p.ID]), r.money(r.running[p.ID]))}
		}
	}
	return Check{"Balances match", true, "ledger agrees with the step-by-step total"}
}

func (r *runner) sumZero(b map[int64]int64) Check {
	var sum int64
	for _, v := range b {
		sum += v
	}
	return Check{"Balances sum to zero", sum == 0, "sum = " + r.money(sum)}
}

// settleUpWorks checks the suggested payments are few enough, all
// positive, go from debtors to creditors, and zero every balance.
func (r *runner) settleUpWorks(b map[int64]int64, pays []ledger.Payment) Check {
	const name = "Settle-up works"
	var nonZero int
	for _, v := range b {
		if v != 0 {
			nonZero++
		}
	}
	if nonZero > 0 && len(pays) > nonZero-1 {
		return Check{name, false, fmt.Sprintf("%d payments for %d people with a balance; at most %d needed", len(pays), nonZero, nonZero-1)}
	}
	after := map[int64]int64{}
	for id, v := range b {
		after[id] = v
	}
	for _, p := range pays {
		if p.Amount <= 0 || b[p.From] >= 0 || b[p.To] <= 0 {
			return Check{name, false, fmt.Sprintf("bad payment %s → %s of %s", r.name(p.From), r.name(p.To), r.money(p.Amount))}
		}
		after[p.From] += p.Amount
		after[p.To] -= p.Amount
	}
	for id, v := range after {
		if v != 0 {
			return Check{name, false, fmt.Sprintf("%s would be left at %s", r.name(id), r.money(v))}
		}
	}
	return Check{name, true, fmt.Sprintf("%d payment(s) clear %d balance(s)", len(pays), nonZero)}
}

func (r *runner) allSquare(b map[int64]int64) Check {
	for _, p := range r.trip.People {
		if b[p.ID] != 0 {
			return Check{"All square", false, fmt.Sprintf("%s ends at %s", p.Name, r.money(b[p.ID]))}
		}
	}
	return Check{"All square", true, "every balance is zero"}
}
