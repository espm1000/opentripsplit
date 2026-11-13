package web

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/espm1000/opentripsplit/internal/auth"
	"github.com/espm1000/opentripsplit/internal/ledger"
	"github.com/espm1000/opentripsplit/internal/store"
)

type groupsView struct {
	base
	Active     []store.GroupSummary
	Archived   []store.GroupSummary
	Currencies []ledger.Currency
	Error      string
	Name       string
	Currency   string
}

func (s *Server) groupsPage(w http.ResponseWriter, r *http.Request, u store.User) {
	s.renderGroups(w, r, u, http.StatusOK, groupsView{Currency: "USD"})
}

func (s *Server) renderGroups(w http.ResponseWriter, r *http.Request, u store.User, status int, v groupsView) {
	gs, err := s.store.GroupsForUser(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, g := range gs {
		if g.Archived() {
			v.Archived = append(v.Archived, g)
		} else {
			v.Active = append(v.Active, g)
		}
	}
	v.base, v.Currencies = s.base(r, "Your groups"), ledger.Currencies
	s.render(w, status, "groups", v)
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, u store.User) {
	name, cur := r.PostFormValue("name"), r.PostFormValue("currency")
	id, err := s.store.CreateGroup(r.Context(), u.ID, name, cur)
	var ue *store.UserError
	if errors.As(err, &ue) {
		s.renderGroups(w, r, u, http.StatusBadRequest, groupsView{Error: capitalize(ue.Msg), Name: name, Currency: cur})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/groups/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// groupData is everything a group page shows, with names resolved.
type groupData struct {
	*groupCtx
	Members     []store.Member
	names       map[int64]string
	Expenses    []expenseRow
	Settlements []settlementRow
	Balances    []balanceRow
	SettleUp    []paymentRow
}

// Name returns a user's display name within the group.
func (d *groupData) Name(id int64) string {
	if n, ok := d.names[id]; ok {
		return n
	}
	return "user #" + strconv.FormatInt(id, 10)
}

// Money formats minor units in the group currency. The no-break space
// keeps the number and currency code on the same line.
func (d *groupData) Money(v int64) string {
	return ledger.FormatAmount(v, d.Group.Decimals()) + "\u00a0" + d.Group.Currency
}

type expenseRow struct {
	store.Expense
	CanEdit bool
}

type settlementRow struct {
	store.Settlement
	CanDelete bool
}

// balanceRow is one line of the high-score board.
type balanceRow struct {
	UserID int64
	Name   string
	Amount int64
	Active bool
	Rank   int // 1 = the person the group owes the most
}

type paymentRow struct {
	ledger.Payment
	CanMark bool
}

func (s *Server) loadGroupData(r *http.Request, gc *groupCtx) (*groupData, error) {
	ctx := r.Context()
	members, err := s.store.Members(ctx, gc.Group.ID)
	if err != nil {
		return nil, err
	}
	expenses, err := s.store.Expenses(ctx, gc.Group.ID)
	if err != nil {
		return nil, err
	}
	settlements, err := s.store.Settlements(ctx, gc.Group.ID)
	if err != nil {
		return nil, err
	}
	d := &groupData{groupCtx: gc, Members: members, names: map[int64]string{}}
	for _, m := range members {
		d.names[m.UserID] = m.Name
	}
	writable := !gc.Group.Archived()
	for _, e := range expenses {
		d.Expenses = append(d.Expenses, expenseRow{e, writable && (e.CreatedBy == gc.User.ID || gc.IsOwner())})
	}
	for _, st := range settlements {
		d.Settlements = append(d.Settlements, settlementRow{st, writable && (st.CreatedBy == gc.User.ID || gc.IsOwner())})
	}

	bal := store.Balances(expenses, settlements)
	for _, m := range members {
		if m.Active || bal[m.UserID] != 0 {
			d.Balances = append(d.Balances, balanceRow{UserID: m.UserID, Name: m.Name, Amount: bal[m.UserID], Active: m.Active})
		}
	}
	// High-score order: most owed first, biggest debt last.
	slices.SortStableFunc(d.Balances, func(a, b balanceRow) int { return cmp.Compare(b.Amount, a.Amount) })
	for i := range d.Balances {
		d.Balances[i].Rank = i + 1
	}
	for _, p := range ledger.SettleUp(bal) {
		canMark := writable && (p.From == gc.User.ID || p.To == gc.User.ID || gc.IsOwner())
		d.SettleUp = append(d.SettleUp, paymentRow{p, canMark})
	}
	return d, nil
}

type groupView struct {
	base
	*groupData
	Invites []store.Invite
}

func (s *Server) groupPage(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	d, err := s.loadGroupData(r, gc)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := groupView{base: s.base(r, gc.Group.Name), groupData: d}
	if gc.IsOwner() && !gc.Group.Archived() {
		if v.Invites, err = s.store.ActiveInvites(r.Context(), gc.Group.ID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	s.render(w, http.StatusOK, "group", v)
}

// done redirects back to the group page, or shows the error.
func (s *Server) done(w http.ResponseWriter, r *http.Request, gc *groupCtx, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, gc.path(), http.StatusSeeOther)
}

func (s *Server) renameGroup(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	s.done(w, r, gc, s.store.RenameGroup(r.Context(), gc.Group.ID, gc.User.ID, r.PostFormValue("name")))
}

func (s *Server) archiveGroup(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	s.done(w, r, gc, s.store.SetArchived(r.Context(), gc.Group.ID, gc.User.ID, true))
}

func (s *Server) unarchiveGroup(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	s.done(w, r, gc, s.store.SetArchived(r.Context(), gc.Group.ID, gc.User.ID, false))
}

func (s *Server) leaveGroup(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	if err := s.store.LeaveGroup(r.Context(), gc.Group.ID, gc.User.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/groups", http.StatusSeeOther)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	uid, ok := pathID(r, "uid")
	if !ok {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	s.done(w, r, gc, s.store.RemoveMember(r.Context(), gc.Group.ID, gc.User.ID, uid))
}

func (s *Server) transferOwnership(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	uid, ok := pathID(r, "uid")
	if !ok {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	s.done(w, r, gc, s.store.TransferOwnership(r.Context(), gc.Group.ID, gc.User.ID, uid))
}

type inviteView struct {
	base
	Group     store.Group
	URL       string
	ExpiresAt string
}

// createInvite makes a new join link and shows it once; only its hash is
// stored.
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	token := auth.NewToken()
	inv, err := s.store.CreateInvite(r.Context(), gc.Group.ID, gc.User.ID, auth.HashToken(token), s.inviteTTL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "invite_created", inviteView{
		base:      s.base(r, "Invite link"),
		Group:     gc.Group,
		URL:       s.baseURL.JoinPath("invite", token).String(),
		ExpiresAt: inv.ExpiresAt.Format("2 Jan 2006 15:04 MST"),
	})
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	iid, ok := pathID(r, "iid")
	if !ok {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	s.done(w, r, gc, s.store.RevokeInvite(r.Context(), gc.Group.ID, gc.User.ID, iid))
}

func (s *Server) invitePage(w http.ResponseWriter, r *http.Request, u store.User) {
	g, err := s.store.InviteGroup(r.Context(), auth.HashToken(r.PathValue("token")))
	if err != nil {
		s.inviteFail(w, r, err)
		return
	}
	if _, err := s.store.Membership(r.Context(), g.ID, u.ID); err == nil {
		http.Redirect(w, r, "/groups/"+strconv.FormatInt(g.ID, 10), http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "invite", inviteView{base: s.base(r, "Join "+g.Name), Group: g})
}

func (s *Server) acceptInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	gid, err := s.store.AcceptInvite(r.Context(), auth.HashToken(r.PathValue("token")), u.ID)
	if err != nil {
		s.inviteFail(w, r, err)
		return
	}
	http.Redirect(w, r, "/groups/"+strconv.FormatInt(gid, 10), http.StatusSeeOther)
}

func (s *Server) inviteFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "This invite link isn't valid. Ask the group owner for a new one.")
	case errors.Is(err, store.ErrArchived):
		s.errorPage(w, r, http.StatusGone, "This group is archived and can't be joined.")
	default:
		s.fail(w, r, err)
	}
}

// historyRow is one audit entry rendered for people.
type historyRow struct {
	store.AuditEntry
	Actor   string
	Summary string
	Details []string
}

type historyView struct {
	base
	*groupData
	Rows []historyRow
}

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	d, err := s.loadGroupData(r, gc)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	entries, err := s.store.AuditLog(r.Context(), gc.Group.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := historyView{base: s.base(r, gc.Group.Name+" history"), groupData: d}
	for _, e := range entries {
		v.Rows = append(v.Rows, describe(d, e))
	}
	s.render(w, http.StatusOK, "history", v)
}
