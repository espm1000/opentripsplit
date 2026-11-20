package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/espm1000/opentripsplit/internal/ledger"
	"github.com/espm1000/opentripsplit/internal/store"
)

// splitFields is the per-person part of the expense form. It is rendered
// inside the form and, on split-type change, alone as an htmx fragment.
type splitFields struct {
	Type     ledger.SplitType
	Members  []store.Member
	Currency string
	Included map[int64]bool   // equal
	Values   map[int64]string // exact, percent, shares
}

func newSplitFields(t ledger.SplitType, g store.Group, members []store.Member) splitFields {
	f := splitFields{Type: t, Members: members, Currency: g.Currency, Included: map[int64]bool{}, Values: map[int64]string{}}
	for _, m := range members {
		switch t {
		case ledger.SplitEqual:
			f.Included[m.UserID] = true
		case ledger.SplitShares:
			f.Values[m.UserID] = "1"
		}
	}
	return f
}

type expenseForm struct {
	Description string
	Amount      string
	PayerID     int64
	SpentOn     string
	Split       splitFields
}

type expenseFormView struct {
	base
	Group      store.Group
	Action     string
	ExpenseID  int64
	Form       expenseForm
	SplitTypes []ledger.SplitType
	Error      string
}

func (s *Server) activeMembers(r *http.Request, gc *groupCtx) ([]store.Member, error) {
	ms, err := s.store.Members(r.Context(), gc.Group.ID)
	var out []store.Member
	for _, m := range ms {
		if m.Active {
			out = append(out, m)
		}
	}
	return out, err
}

func (s *Server) renderExpenseForm(w http.ResponseWriter, r *http.Request, gc *groupCtx, status int, expenseID int64, f expenseForm, errMsg string) {
	v := expenseFormView{
		Group: gc.Group, ExpenseID: expenseID, Form: f, SplitTypes: ledger.SplitTypes, Error: errMsg,
		Action: gc.path("/expenses"),
	}
	if expenseID != 0 {
		v.Action = gc.path("/expenses/", strconv.FormatInt(expenseID, 10))
		v.base = s.base(r, "Edit expense")
	} else {
		v.base = s.base(r, "Add expense")
	}
	s.render(w, status, "expense_form", v)
}

func (s *Server) newExpensePage(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	if gc.Group.Archived() {
		s.fail(w, r, store.ErrArchived)
		return
	}
	members, err := s.activeMembers(r, gc)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f := expenseForm{
		PayerID: gc.User.ID,
		SpentOn: time.Now().Format(time.DateOnly),
		Split:   newSplitFields(ledger.SplitEqual, gc.Group, members),
	}
	s.renderExpenseForm(w, r, gc, http.StatusOK, 0, f, "")
}

func (s *Server) editExpensePage(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	e, ok := s.editableExpense(w, r, gc)
	if !ok {
		return
	}
	members, err := s.activeMembers(r, gc)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if includesFormer(e, members) {
		s.fail(w, r, store.ErrFormerParticipant)
		return
	}
	dec := gc.Group.Decimals()
	f := expenseForm{
		Description: e.Description,
		Amount:      ledger.FormatAmount(e.Amount, dec),
		PayerID:     e.PayerID,
		SpentOn:     e.SpentOn,
		Split:       splitFields{Type: e.SplitType, Members: members, Currency: gc.Group.Currency, Included: map[int64]bool{}, Values: map[int64]string{}},
	}
	for _, sh := range e.Shares {
		switch e.SplitType {
		case ledger.SplitEqual:
			f.Split.Included[sh.UserID] = true
		case ledger.SplitExact:
			f.Split.Values[sh.UserID] = ledger.FormatAmount(sh.Input, dec)
		case ledger.SplitPercent:
			f.Split.Values[sh.UserID] = ledger.FormatAmount(sh.Input, 2)
		case ledger.SplitShares:
			f.Split.Values[sh.UserID] = strconv.FormatInt(sh.Input, 10)
		}
	}
	s.renderExpenseForm(w, r, gc, http.StatusOK, e.ID, f, "")
}

// editableExpense loads the {eid} expense and checks the user may change
// it. The store re-checks on write; this gives a clean error on GET.
func (s *Server) editableExpense(w http.ResponseWriter, r *http.Request, gc *groupCtx) (store.Expense, bool) {
	eid, ok := pathID(r, "eid")
	if !ok {
		s.fail(w, r, store.ErrNotFound)
		return store.Expense{}, false
	}
	e, err := s.store.Expense(r.Context(), gc.Group.ID, eid)
	switch {
	case err != nil:
		s.fail(w, r, err)
		return e, false
	case gc.Group.Archived():
		s.fail(w, r, store.ErrArchived)
		return e, false
	case e.CreatedBy != gc.User.ID && !gc.IsOwner():
		s.fail(w, r, store.ErrForbidden)
		return e, false
	}
	return e, true
}

// includesFormer reports whether the payer or anyone with a share is not an
// active member. Such expenses can't be edited (see store.ErrFormerParticipant).
func includesFormer(e store.Expense, members []store.Member) bool {
	active := map[int64]bool{}
	for _, m := range members {
		active[m.UserID] = m.Active
	}
	for _, id := range e.Participants() {
		if !active[id] {
			return true
		}
	}
	return false
}

// splitFields renders just the per-person inputs for the chosen split type
// (htmx swaps them in when the split-type select changes).
func (s *Server) splitFields(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	t := ledger.SplitType(r.URL.Query().Get("split_type"))
	if !t.Valid() {
		t = ledger.SplitEqual
	}
	members, err := s.activeMembers(r, gc)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderTemplate(w, http.StatusOK, "expense_form", "split_fields", newSplitFields(t, gc.Group, members))
}

// parseExpenseForm reads the form into both its raw values (to re-render on
// error) and a store input. Amounts are parsed without floating point.
func parseExpenseForm(r *http.Request, g store.Group, members []store.Member) (expenseForm, store.ExpenseInput, error) {
	t := ledger.SplitType(r.PostFormValue("split_type"))
	f := expenseForm{
		Description: r.PostFormValue("description"),
		Amount:      strings.TrimSpace(r.PostFormValue("amount")),
		PayerID:     formID(r, "payer_id"),
		SpentOn:     r.PostFormValue("spent_on"),
		Split:       splitFields{Type: t, Members: members, Currency: g.Currency, Included: map[int64]bool{}, Values: map[int64]string{}},
	}
	in := store.ExpenseInput{Description: f.Description, PayerID: f.PayerID, SpentOn: f.SpentOn, SplitType: t}
	if !t.Valid() {
		f.Split.Type = ledger.SplitEqual
		return f, in, errors.New("pick how to split the expense")
	}

	dec := g.Decimals()
	var err error
	if in.Amount, err = ledger.ParseAmount(f.Amount, dec); err != nil {
		return f, in, errors.New("amount: " + err.Error())
	}
	for _, m := range members {
		key := strconv.FormatInt(m.UserID, 10)
		if t == ledger.SplitEqual {
			if r.PostFormValue("include_"+key) != "" {
				f.Split.Included[m.UserID] = true
				in.Shares = append(in.Shares, ledger.Share{UserID: m.UserID})
			}
			continue
		}
		raw := strings.TrimSpace(r.PostFormValue("value_" + key))
		f.Split.Values[m.UserID] = raw
		if raw == "" {
			continue
		}
		var v int64
		var perr error
		switch t {
		case ledger.SplitExact:
			v, perr = ledger.ParseAmount(raw, dec)
		case ledger.SplitPercent:
			v, perr = ledger.ParseAmount(raw, 2)
		case ledger.SplitShares:
			v, perr = strconv.ParseInt(raw, 10, 64)
			if perr != nil || v <= 0 {
				perr = ledger.ErrShareWeight
			}
		}
		if perr != nil {
			if err == nil {
				err = errors.New(m.Name + ": " + perr.Error())
			}
			continue
		}
		in.Shares = append(in.Shares, ledger.Share{UserID: m.UserID, Input: v})
	}
	return f, in, err
}

func (s *Server) saveExpense(w http.ResponseWriter, r *http.Request, gc *groupCtx, expenseID int64) {
	members, err := s.activeMembers(r, gc)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f, in, err := parseExpenseForm(r, gc.Group, members)
	if err != nil {
		s.renderExpenseForm(w, r, gc, http.StatusBadRequest, expenseID, f, capitalize(err.Error())+".")
		return
	}
	if expenseID == 0 {
		_, err = s.store.CreateExpense(r.Context(), gc.Group.ID, gc.User.ID, in)
	} else {
		err = s.store.UpdateExpense(r.Context(), gc.Group.ID, expenseID, gc.User.ID, in)
	}
	var ue *store.UserError
	switch {
	case err == nil:
		http.Redirect(w, r, gc.path(), http.StatusSeeOther)
	case errors.As(err, &ue):
		s.renderExpenseForm(w, r, gc, http.StatusBadRequest, expenseID, f, capitalize(ue.Msg)+".")
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) createExpense(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	s.saveExpense(w, r, gc, 0)
}

func (s *Server) updateExpense(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	eid, ok := pathID(r, "eid")
	if !ok {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	s.saveExpense(w, r, gc, eid)
}

func (s *Server) deleteExpense(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	eid, ok := pathID(r, "eid")
	if !ok {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	s.done(w, r, gc, s.store.DeleteExpense(r.Context(), gc.Group.ID, eid, gc.User.ID))
}

func (s *Server) createSettlement(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	amount, err := ledger.ParseAmount(r.PostFormValue("amount"), gc.Group.Decimals())
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "Amount: "+err.Error()+".")
		return
	}
	_, err = s.store.CreateSettlement(r.Context(), gc.Group.ID, gc.User.ID, formID(r, "from"), formID(r, "to"), amount)
	s.done(w, r, gc, err)
}

func (s *Server) deleteSettlement(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	sid, ok := pathID(r, "sid")
	if !ok {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	s.done(w, r, gc, s.store.DeleteSettlement(r.Context(), gc.Group.ID, sid, gc.User.ID))
}
