package web

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/espm1000/opentripsplit/internal/ledger"
	"github.com/espm1000/opentripsplit/internal/sim"
)

// The simulator (SIMULATOR) plays random trips through the ledger code and
// shows the result, for demos and for eyeballing the math. It is public,
// stateless and saves nothing: every request replays the trip from its seed
// up to step "at" (internal/sim does the work). Its route isn't registered
// at all when it's off.
//
// Paging and autoplay are htmx requests that swap #sim-stage (controls and
// the transfer window) and update the terminal, board and checks out of
// band. Everything shown, including which animations play, is decided
// here; the browser only plays what the server sends.

// Defaults for a new trip.
const (
	simPeople   = 4
	simExpenses = 12
)

// simDelay is the autoplay pause between steps. sim.js types each terminal
// block within simTypeMS (several characters per frame when a block is
// long), so typing finishes before the next step arrives.
const (
	simDelay  = "2400ms"
	simTypeMS = 1500
)

func (s *Server) registerSimRoutes(mux *http.ServeMux) {
	if !s.simulator {
		return
	}
	mux.HandleFunc("GET /simulator", s.simulatorPage)
}

// simQuery is a simulator URL: a trip, how far into it we are, and how
// it is shown.
type simQuery struct {
	sim.Params
	At     int
	Play   bool
	Checks bool // show the math checks panel (hidden by default)
}

func (q simQuery) URL() string {
	v := url.Values{}
	v.Set("seed", strconv.FormatUint(q.Seed, 10))
	v.Set("people", strconv.Itoa(q.People))
	v.Set("expenses", strconv.Itoa(q.Expenses))
	if q.Currency != "" {
		v.Set("currency", q.Currency)
	}
	v.Set("at", strconv.Itoa(q.At))
	if q.Play {
		v.Set("play", "1")
	}
	if q.Checks {
		v.Set("checks", "1")
	}
	return "/simulator?" + v.Encode()
}

// with returns q moved to step at, playing or not.
func (q simQuery) with(at int, play bool) simQuery {
	q.At, q.Play = at, play
	return q
}

var (
	errSimParams   = errors.New("seed, people, expenses and at must be whole numbers")
	errSimCurrency = errors.New("unknown currency")
)

// parseSimQuery reads a simulator URL. People and Expenses are clamped;
// At is clamped once the trip's length is known.
func parseSimQuery(v url.Values) (simQuery, error) {
	q := simQuery{Params: sim.Params{People: simPeople, Expenses: simExpenses}, Play: v.Get("play") == "1", Checks: v.Get("checks") == "1"}
	num := func(key string, dst *int) error {
		if s := v.Get(key); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				return errSimParams
			}
			*dst = n
		}
		return nil
	}
	if err := errors.Join(num("people", &q.People), num("expenses", &q.Expenses), num("at", &q.At)); err != nil {
		return q, errSimParams
	}
	if s := v.Get("seed"); s != "" {
		seed, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return q, errSimParams
		}
		q.Seed = seed
	}
	if c := strings.ToUpper(strings.TrimSpace(v.Get("currency"))); c != "" {
		if _, ok := ledger.LookupCurrency(c); !ok {
			return q, errSimCurrency
		}
		q.Currency = c
	}
	q.Params = q.Clamp()
	return q, nil
}

// simActor is someone (or a shop) in the transfer window. Sprite names a
// class in app.css that draws a pixelarticons sprite.
type simActor struct {
	Name   string
	Sprite string // human, human-up or store
}

// simScene is what the transfer window shows: coins going From → To, or
// the whole party standing around before the start and after the end.
type simScene struct {
	Title   string
	Caption string
	From    simActor
	To      simActor
	Party   []simActor // set instead of From/To
	Cheer   bool       // the party celebrates (trip finished)
}

// termLine is one line in the terminal. Class is "", prompt, ok or fail.
type termLine struct {
	Text  string
	Class string
}

type simFailure struct {
	Step  int
	Check sim.Check
}

// simExpenseRow and simPaymentRow mirror the Expenses and Payments cards on
// the group page. New marks the row added at the current step.
type simExpenseRow struct {
	Day         int
	Description string
	Split       string
	Payer       string
	Amount      string
	New         bool
}

type simPaymentRow struct {
	Day      int
	From     string
	To       string
	Amount   string
	SettleUp bool
	New      bool
}

type simView struct {
	base
	Q          simQuery
	Trip       sim.Trip
	Currencies []ledger.Currency
	Steps      int // events in the trip
	Done       bool
	Scene      simScene
	Say        string // the step in one sentence, for screen readers
	Board      []balanceRow
	Changed    map[int64]int64
	Suggest    []string
	Expenses   []simExpenseRow // newest first
	Payments   []simPaymentRow // newest first
	Checks     []sim.Check
	Passed     int // checks passed in steps 1..At
	Total      int
	Failures   []simFailure
	// Term is the terminal: the boot block, then one block per step. Only
	// the last block is typed out.
	Term   [][]termLine
	Delay  string
	TypeMS int // time budget for typing out one terminal block
	// Partial is an htmx response: #sim-stage, plus out-of-band updates
	// of the rest. After a single step forward TermAdd appends just the
	// new terminal block. On the same step (Pause, the checks toggle)
	// TermKeep leaves the terminal alone, so it isn't typed out again.
	// Otherwise the whole terminal is replaced.
	Partial  bool
	TermAdd  bool
	TermKeep bool
}

// ShowChecks says whether the math checks panel is shown. A failed check
// always shows it.
func (v *simView) ShowChecks() bool { return v.Q.Checks || len(v.Failures) > 0 }

func (v *simView) Money(n int64) string {
	return ledger.FormatAmount(n, v.Trip.Currency.Decimals) + "\u00a0" + v.Trip.Currency.Code
}

func (v *simView) Name(id int64) string {
	return v.Trip.People[id-1].Name
}

// LastBlock is the terminal block for the current step; it is the one
// typed out.
func (v *simView) LastBlock() []termLine { return v.Term[len(v.Term)-1] }

func (v *simView) IsLast(i int) bool { return i == len(v.Term)-1 }

func (v *simView) link(at int, play bool) string { return v.Q.with(at, play).URL() }

// Links for the controls. Play steps forward at once and keeps autoplay
// on, so autoplay itself follows PlayLink.
func (v *simView) StepLink() string    { return v.link(v.Q.At+1, false) }
func (v *simView) PlayLink() string    { return v.link(v.Q.At+1, true) }
func (v *simView) PauseLink() string   { return v.link(v.Q.At, false) }
func (v *simView) RestartLink() string { return v.link(0, false) }
func (v *simView) ReplayLink() string  { return v.link(1, true) }
func (v *simView) EndLink() string     { return v.link(v.Steps, false) }

// ChecksLink flips the math checks panel, staying on the same step.
func (v *simView) ChecksLink() string {
	q := v.Q
	q.Checks = !q.Checks
	return q.URL()
}

// NewTrip is a link to a fresh seed with the same settings.
func (v *simView) NewTrip() string {
	q := url.Values{"people": {strconv.Itoa(v.Q.People)}, "expenses": {strconv.Itoa(v.Q.Expenses)}}
	if v.Q.Currency != "" {
		q.Set("currency", v.Q.Currency)
	}
	if v.Q.Checks {
		q.Set("checks", "1")
	}
	return "/simulator?" + q.Encode()
}

func (s *Server) simulatorPage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q, err := parseSimQuery(query)
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	if !query.Has("seed") {
		// Short seeds read well in a demo and are easy to share.
		q.Seed = rand.Uint64N(1_000_000)                                 // #nosec G404 -- picks a demo trip, not a secret
		http.Redirect(w, r, q.with(0, false).URL(), http.StatusSeeOther) // #nosec G710 -- a fixed local path; the query holds only values we formatted
		return
	}

	trip := sim.Run(q.Params)
	q.At = min(max(q.At, 0), len(trip.Events))
	v := &simView{base: s.base(r, "Trip simulator"), Q: q, Trip: trip, Currencies: ledger.Currencies, Steps: len(trip.Events), Delay: simDelay, TypeMS: simTypeMS}
	v.Done = q.At == v.Steps
	v.Q.Play = q.Play && !v.Done
	v.build()
	if len(v.Failures) > 0 {
		f := v.Failures[0]
		s.log.Warn("simulator check failed", "seed", q.Seed, "people", q.People, "expenses", q.Expenses, "currency", trip.Currency.Code,
			"step", f.Step, "check", f.Check.Name, "detail", f.Check.Detail)
	}

	w.Header().Set("Vary", "HX-Request")
	// A history restore after a cache miss wants the whole page.
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
		v.Partial = true
		if prev, ok := simCurrent(r); ok && prev.Params == q.Params {
			// prev.At comes straight from the URL, so clamp it like q.At.
			switch min(max(prev.At, 0), v.Steps) {
			case q.At - 1:
				v.TermAdd = true
			case q.At:
				v.TermKeep = true
			}
		}
		s.renderTemplate(w, http.StatusOK, "simulator", "sim_step", v)
		return
	}
	s.render(w, http.StatusOK, "simulator", v)
}

// simCurrent reads the simulator URL the browser is showing, which htmx
// sends with every request.
func simCurrent(r *http.Request) (simQuery, bool) {
	u, err := url.Parse(r.Header.Get("HX-Current-URL"))
	if err != nil || u.Path != "/simulator" || !u.Query().Has("seed") {
		return simQuery{}, false
	}
	q, err := parseSimQuery(u.Query())
	return q, err == nil
}

// build fills in everything shown at step v.Q.At.
func (v *simView) build() {
	events := v.Trip.Events[:v.Q.At]
	settles := 0
	for _, ev := range v.Trip.Events {
		if ev.Kind == sim.KindSettle {
			settles++
		}
	}

	v.Term = append(v.Term, v.boot())
	settle := 0
	for _, ev := range events {
		for _, c := range ev.Checks {
			v.Total++
			if c.OK {
				v.Passed++
			} else {
				v.Failures = append(v.Failures, simFailure{ev.N, c})
			}
		}
		if ev.Kind == sim.KindSettle {
			settle++
		}
		v.Term = append(v.Term, v.termBlock(ev, settle, settles))
	}
	for i := len(events) - 1; i >= 0; i-- {
		v.addRow(events[i], i == len(events)-1)
	}

	balances := map[int64]int64{}
	var suggested []ledger.Payment
	switch {
	case len(events) == 0:
		v.Scene = v.party(false)
		v.Say = "Ready. Press Step or Play."
	default:
		ev := events[len(events)-1]
		v.Checks, v.Changed, balances, suggested = ev.Checks, ev.Changed, ev.Balances, ev.Suggested
		v.Scene = v.scene(ev, settle, settles)
		v.Say = v.Scene.Title + ". " + v.Scene.Caption
		if v.Done {
			v.Scene = v.party(true)
			v.Say += ". " + v.Scene.Title + "."
			last := &v.Term[len(v.Term)-1]
			if len(v.Failures) > 0 {
				*last = append(*last, termLine{fmt.Sprintf("LOAD FAILED: %d check(s) failed. Replay seed %d.", len(v.Failures), v.Q.Seed), "fail"})
			} else {
				*last = append(*last, termLine{"ALL ASSETS LOADED. Everyone is square.", "ok"})
			}
		}
	}
	for _, p := range v.Trip.People {
		v.Board = append(v.Board, balanceRow{UserID: p.ID, Name: p.Name, Amount: balances[p.ID], Active: true})
	}
	rankBalances(v.Board)
	for _, p := range suggested {
		v.Suggest = append(v.Suggest, fmt.Sprintf("%s pays %s %s", v.Name(p.From), v.Name(p.To), v.Money(p.Amount)))
	}
}

func (v *simView) party(cheer bool) simScene {
	sc := simScene{Cheer: cheer, Title: "Ready", Caption: fmt.Sprintf("%d travelers, %d expenses, paid in %s", len(v.Trip.People), v.Q.Expenses, v.Trip.Currency.Code)}
	sprite := "human"
	if cheer {
		sprite = "human-up"
		sc.Title, sc.Caption = "Trip complete", "Everyone is square"
		if len(v.Failures) > 0 {
			sc.Caption = "Some math checks failed"
		}
	}
	for _, p := range v.Trip.People {
		sc.Party = append(sc.Party, simActor{p.Name, sprite})
	}
	return sc
}

func (v *simView) scene(ev sim.Event, settle, settles int) simScene {
	if e := ev.Expense; e != nil {
		return simScene{
			Title:   "Paying " + v.Money(e.Amount),
			Caption: fmt.Sprintf("%s paid for %s: %s", v.Name(e.PayerID), strings.ToLower(e.Description), splitSummary(e)),
			From:    simActor{v.Name(e.PayerID), "human"},
			To:      simActor{e.Description, "store"},
		}
	}
	p := ev.Payment
	sc := simScene{
		Title: "Transferring " + v.Money(p.Amount),
		From:  simActor{v.Name(p.From), "human"},
		To:    simActor{v.Name(p.To), "human-up"},
	}
	if ev.Kind == sim.KindPayment {
		sc.Caption = fmt.Sprintf("%s paid %s back part of a debt", v.Name(p.From), v.Name(p.To))
	} else {
		sc.Caption = fmt.Sprintf("Settle-up payment %d of %d: %s pays %s", settle, settles, v.Name(p.From), v.Name(p.To))
	}
	return sc
}

// addRow adds ev to the Expenses or Payments card.
func (v *simView) addRow(ev sim.Event, isNew bool) {
	if e := ev.Expense; e != nil {
		parts := make([]string, len(e.Portions))
		for i, p := range e.Portions {
			parts[i] = v.Name(p.UserID) + " " + v.Money(p.Amount)
		}
		v.Expenses = append(v.Expenses, simExpenseRow{
			Day: ev.Day, Description: e.Description, Split: string(e.SplitType) + ": " + strings.Join(parts, ", "),
			Payer: v.Name(e.PayerID), Amount: v.Money(e.Amount), New: isNew,
		})
		return
	}
	p := ev.Payment
	v.Payments = append(v.Payments, simPaymentRow{
		Day: ev.Day, From: v.Name(p.From), To: v.Name(p.To), Amount: v.Money(p.Amount),
		SettleUp: ev.Kind == sim.KindSettle, New: isNew,
	})
}

func splitSummary(e *sim.Expense) string {
	people := "people"
	if len(e.Shares) == 1 {
		people = "person"
	}
	return fmt.Sprintf("%s split between %d %s", e.SplitType, len(e.Shares), people)
}

// boot is the terminal's first block, shown before step 1.
func (v *simView) boot() []termLine {
	names := make([]string, len(v.Trip.People))
	for i, p := range v.Trip.People {
		names[i] = p.Name
	}
	return []termLine{
		{fmt.Sprintf(`C:\TRIPS> trip.exe /seed:%d /currency:%s`, v.Q.Seed, v.Trip.Currency.Code), "prompt"},
		{fmt.Sprintf("Loading TRIP.SAV ... %d players, %d expenses, %d steps", len(v.Trip.People), v.Q.Expenses, v.Steps), ""},
		{"Players: " + strings.Join(names, ", "), ""},
		{"[ OK ] Ready. Press Step or Play.", "ok"},
	}
}

// termBlock logs one step as if it were loading a game asset.
func (v *simView) termBlock(ev sim.Event, settle, settles int) []termLine {
	asset, out := "", []termLine{}
	switch {
	case ev.Expense != nil:
		e := ev.Expense
		asset = "EXPENSE_" + strings.ToUpper(strings.ReplaceAll(e.Description, " ", "_"))
		out = append(out, termLine{fmt.Sprintf("  %s paid %s for %s", v.Name(e.PayerID), v.Money(e.Amount), strings.ToLower(e.Description)), ""})
		var parts []string
		inputs := map[int64]int64{}
		for _, sh := range e.Shares {
			inputs[sh.UserID] = sh.Input
		}
		for _, p := range e.Portions {
			part := v.Name(p.UserID) + " " + ledger.FormatAmount(p.Amount, v.Trip.Currency.Decimals)
			switch e.SplitType {
			case ledger.SplitPercent:
				part += " (" + ledger.FormatAmount(inputs[p.UserID], 2) + "%)"
			case ledger.SplitShares:
				part += fmt.Sprintf(" (%d sh)", inputs[p.UserID])
			}
			parts = append(parts, part)
		}
		out = append(out, termLine{"  " + splitSummary(e) + ": " + strings.Join(parts, " · "), ""})
	case ev.Kind == sim.KindPayment:
		asset = "PAYBACK"
		out = append(out, termLine{fmt.Sprintf("  %s paid %s back %s", v.Name(ev.Payment.From), v.Name(ev.Payment.To), v.Money(ev.Payment.Amount)), ""})
	default:
		asset = fmt.Sprintf("SETTLE_UP_%d_OF_%d", settle, settles)
		out = append(out, termLine{fmt.Sprintf("  %s paid %s %s", v.Name(ev.Payment.From), v.Name(ev.Payment.To), v.Money(ev.Payment.Amount)), ""})
	}
	head := termLine{fmt.Sprintf("> LOAD %0*d/%d %s.OTS", len(strconv.Itoa(v.Steps)), ev.N, v.Steps, asset), "prompt"}
	out = append([]termLine{head}, out...)

	failed := ev.Failed()
	for _, c := range failed {
		out = append(out, termLine{"  [FAIL] " + c.Name + ": " + c.Detail, "fail"})
	}
	if len(failed) == 0 {
		out = append(out, termLine{fmt.Sprintf("  [ OK ] %d/%d checks passed", len(ev.Checks), len(ev.Checks)), "ok"})
	}
	return out
}
