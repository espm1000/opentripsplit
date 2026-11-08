package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/espm1000/opentripsplit/internal/ledger"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := &clock{time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
	s.Now = c.now
	return s, c
}

func newUser(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	u, err := s.LoginIdentity(context.Background(), "github", name+"-id", name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// groupWith creates a group owned by the first user and joins the rest.
func groupWith(t *testing.T, s *Store, owner int64, others ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	g, err := s.CreateGroup(ctx, owner, "Lisbon", "EUR")
	if err != nil {
		t.Fatal(err)
	}
	tok := hash("invite-" + time.Now().String())
	if _, err := s.CreateInvite(ctx, g, owner, tok, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, u := range others {
		if _, err := s.AcceptInvite(ctx, tok, u); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func equal(payer int64, amount int64, users ...int64) ExpenseInput {
	in := ExpenseInput{PayerID: payer, Description: "Dinner", Amount: amount, SplitType: ledger.SplitEqual, SpentOn: "2026-07-01"}
	for _, u := range users {
		in.Shares = append(in.Shares, ledger.Share{UserID: u})
	}
	return in
}

func balances(t *testing.T, s *Store, g int64) map[int64]int64 {
	t.Helper()
	b, err := groupBalances(context.Background(), s.db, g)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestForeignKeysEnforced(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.db.Exec(`INSERT INTO group_members (group_id, user_id, role, joined_at) VALUES (999, 999, 'member', 0)`)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("expected foreign key error, got %v", err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for range 2 {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

func TestLoginAndLink(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	u1, err := s.LoginIdentity(ctx, "github", "42", "  Ana   Lima ")
	if err != nil {
		t.Fatal(err)
	}
	if u1.DisplayName != "Ana Lima" {
		t.Fatalf("name = %q", u1.DisplayName)
	}
	again, _ := s.LoginIdentity(ctx, "github", "42", "renamed upstream")
	if again.ID != u1.ID || again.DisplayName != "Ana Lima" {
		t.Fatalf("second login = %+v", again)
	}
	if err := s.LinkIdentity(ctx, u1.ID, "discord", "d1"); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkIdentity(ctx, u1.ID, "discord", "d1"); err != nil {
		t.Fatalf("relinking own identity: %v", err)
	}
	viaDiscord, _ := s.LoginIdentity(ctx, "discord", "d1", "x")
	if viaDiscord.ID != u1.ID {
		t.Fatal("discord login didn't reach the linked user")
	}
	u2, _ := s.LoginIdentity(ctx, "github", "43", "Bo")
	if err := s.LinkIdentity(ctx, u2.ID, "discord", "d1"); !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("err = %v, want ErrIdentityTaken", err)
	}
	ids, _ := s.Identities(ctx, u1.ID)
	if len(ids) != 2 {
		t.Fatalf("identities = %v", ids)
	}
	blank, _ := s.LoginIdentity(ctx, "discord", "d2", "   ")
	if blank.DisplayName != "discord user" {
		t.Fatalf("blank name fallback = %q", blank.DisplayName)
	}
}

func TestSessions(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	u := newUser(t, s, "ana")
	if err := s.CreateSession(ctx, hash("a"), u, c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.SessionUser(ctx, hash("a")); err != nil || got.ID != u {
		t.Fatalf("SessionUser = %+v, %v", got, err)
	}
	if _, err := s.SessionUser(ctx, hash("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session err = %v", err)
	}
	c.t = c.t.Add(2 * time.Hour)
	if _, err := s.SessionUser(ctx, hash("a")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session err = %v", err)
	}
	if n, err := s.PurgeExpiredSessions(ctx); err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
}

func TestExpenseLifecycleAndPermissions(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ana, bo, cy := newUser(t, s, "ana"), newUser(t, s, "bo"), newUser(t, s, "cy")
	g := groupWith(t, s, ana, bo, cy)

	id, err := s.CreateExpense(ctx, g, bo, equal(bo, 900, ana, bo, cy))
	if err != nil {
		t.Fatal(err)
	}
	if b := balances(t, s, g); b[bo] != 600 || b[ana] != -300 || b[cy] != -300 {
		t.Fatalf("balances = %v", b)
	}

	// Only the creator (bo) or owner (ana) may edit.
	if err := s.UpdateExpense(ctx, g, id, cy, equal(bo, 600, ana, bo, cy)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cy edit err = %v", err)
	}
	pct := ExpenseInput{PayerID: bo, Description: "Taxi", Amount: 1000, SplitType: ledger.SplitPercent, SpentOn: "2026-07-02",
		Shares: []ledger.Share{{UserID: ana, Input: 2500}, {UserID: bo, Input: 7500}}}
	if err := s.UpdateExpense(ctx, g, id, ana, pct); err != nil {
		t.Fatal(err)
	}
	e, err := s.Expense(ctx, g, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.Description != "Taxi" || len(e.Shares) != 2 || e.Shares[0].Input != 2500 || e.Shares[0].Amount != 250 {
		t.Fatalf("expense after edit = %+v", e)
	}

	// Validation surfaces as UserError.
	bad := pct
	bad.Shares = []ledger.Share{{UserID: ana, Input: 5000}}
	var ue *UserError
	if err := s.UpdateExpense(ctx, g, id, ana, bad); !errors.As(err, &ue) {
		t.Fatalf("bad percent err = %v", err)
	}
	outsider := newUser(t, s, "dee")
	if _, err := s.CreateExpense(ctx, g, ana, equal(ana, 100, ana, outsider)); !errors.As(err, &ue) {
		t.Fatalf("non-member participant err = %v", err)
	}
	if _, err := s.CreateExpense(ctx, g, outsider, equal(ana, 100, ana)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-member actor err = %v", err)
	}

	if err := s.DeleteExpense(ctx, g, id, cy); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cy delete err = %v", err)
	}
	if err := s.DeleteExpense(ctx, g, id, bo); err != nil {
		t.Fatal(err)
	}
	if es, _ := s.Expenses(ctx, g); len(es) != 0 {
		t.Fatalf("expenses after delete = %v", es)
	}
	log, _ := s.AuditLog(ctx, g)
	var actions []string
	for _, a := range log {
		if a.Entity == "expense" {
			actions = append(actions, a.Action)
		}
	}
	if strings.Join(actions, ",") != "delete,update,create" {
		t.Fatalf("expense audit actions = %v", actions)
	}
}

func TestExpenseFromOtherGroupNotFound(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ana, bo := newUser(t, s, "ana"), newUser(t, s, "bo")
	g1 := groupWith(t, s, ana)
	g2 := groupWith(t, s, bo)
	id, err := s.CreateExpense(ctx, g1, ana, equal(ana, 100, ana))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExpense(ctx, g2, id, bo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group delete err = %v", err)
	}
}

func TestSettlements(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ana, bo, cy := newUser(t, s, "ana"), newUser(t, s, "bo"), newUser(t, s, "cy")
	g := groupWith(t, s, ana, bo, cy)
	if _, err := s.CreateExpense(ctx, g, ana, equal(ana, 900, ana, bo, cy)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSettlement(ctx, g, cy, bo, ana, 300); !errors.Is(err, ErrForbidden) {
		t.Fatalf("third party settlement err = %v", err)
	}
	sid, err := s.CreateSettlement(ctx, g, bo, bo, ana, 300)
	if err != nil {
		t.Fatal(err)
	}
	if b := balances(t, s, g); b[bo] != 0 || b[ana] != 300 {
		t.Fatalf("balances = %v", b)
	}
	if _, err := s.CreateSettlement(ctx, g, bo, bo, bo, 1); err == nil {
		t.Fatal("self settlement allowed")
	}
	if err := s.DeleteSettlement(ctx, g, sid, cy); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cy delete settlement err = %v", err)
	}
	if err := s.DeleteSettlement(ctx, g, sid, ana); err != nil {
		t.Fatal(err)
	}
	if b := balances(t, s, g); b[bo] != -300 {
		t.Fatalf("balances after delete = %v", b)
	}
}

func TestLeaveRequiresZeroBalance(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ana, bo := newUser(t, s, "ana"), newUser(t, s, "bo")
	g := groupWith(t, s, ana, bo)
	if _, err := s.CreateExpense(ctx, g, ana, equal(ana, 200, ana, bo)); err != nil {
		t.Fatal(err)
	}
	if err := s.LeaveGroup(ctx, g, bo); !errors.Is(err, ErrNonZeroBalance) {
		t.Fatalf("leave err = %v", err)
	}
	if err := s.RemoveMember(ctx, g, ana, bo); !errors.Is(err, ErrNonZeroBalance) {
		t.Fatalf("remove err = %v", err)
	}
	if err := s.LeaveGroup(ctx, g, ana); !errors.Is(err, ErrOwnerMustStay) {
		t.Fatalf("owner leave err = %v", err)
	}
	if _, err := s.CreateSettlement(ctx, g, bo, bo, ana, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.LeaveGroup(ctx, g, bo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Membership(ctx, g, bo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("membership after leave err = %v", err)
	}
	ms, _ := s.Members(ctx, g)
	if len(ms) != 2 || ms[1].UserID != bo || ms[1].Active {
		t.Fatalf("members = %+v", ms)
	}
}

func TestRemoveAndTransfer(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ana, bo, cy := newUser(t, s, "ana"), newUser(t, s, "bo"), newUser(t, s, "cy")
	g := groupWith(t, s, ana, bo, cy)
	if err := s.RemoveMember(ctx, g, bo, cy); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member removing member err = %v", err)
	}
	if err := s.TransferOwnership(ctx, g, ana, bo); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Membership(ctx, g, bo); m.Role != RoleOwner {
		t.Fatalf("bo role = %s", m.Role)
	}
	if m, _ := s.Membership(ctx, g, ana); m.Role != RoleMember {
		t.Fatalf("ana role = %s", m.Role)
	}
	if err := s.RemoveMember(ctx, g, bo, cy); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedGroupIsReadOnly(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ana, bo := newUser(t, s, "ana"), newUser(t, s, "bo")
	g := groupWith(t, s, ana, bo)
	id, _ := s.CreateExpense(ctx, g, ana, equal(ana, 200, ana, bo))
	if err := s.SetArchived(ctx, g, bo, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member archive err = %v", err)
	}
	if err := s.SetArchived(ctx, g, ana, true); err != nil {
		t.Fatal(err)
	}
	checks := map[string]error{
		"create expense": func() error { _, err := s.CreateExpense(ctx, g, ana, equal(ana, 1, ana)); return err }(),
		"update expense": s.UpdateExpense(ctx, g, id, ana, equal(ana, 1, ana)),
		"delete expense": s.DeleteExpense(ctx, g, id, ana),
		"settle":         func() error { _, err := s.CreateSettlement(ctx, g, bo, bo, ana, 100); return err }(),
		"rename":         s.RenameGroup(ctx, g, ana, "x"),
		"invite":         func() error { _, err := s.CreateInvite(ctx, g, ana, hash("z"), time.Hour); return err }(),
		"leave":          s.LeaveGroup(ctx, g, bo),
	}
	for name, err := range checks {
		if !errors.Is(err, ErrArchived) {
			t.Errorf("%s on archived group: err = %v", name, err)
		}
	}
	if err := s.SetArchived(ctx, g, ana, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateExpense(ctx, g, ana, equal(ana, 1, ana)); err != nil {
		t.Fatalf("after unarchive: %v", err)
	}
}

func TestInvites(t *testing.T) {
	s, c := newStore(t)
	ctx := context.Background()
	ana, bo := newUser(t, s, "ana"), newUser(t, s, "bo")
	g, _ := s.CreateGroup(ctx, ana, "Trip", "JPY")

	if _, err := s.CreateInvite(ctx, g, bo, hash("x"), time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-member invite err = %v", err)
	}
	inv, err := s.CreateInvite(ctx, g, ana, hash("t1"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InviteGroup(ctx, hash("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown invite err = %v", err)
	}
	if gr, err := s.InviteGroup(ctx, hash("t1")); err != nil || gr.ID != g {
		t.Fatalf("InviteGroup = %+v, %v", gr, err)
	}
	if got, err := s.AcceptInvite(ctx, hash("t1"), bo); err != nil || got != g {
		t.Fatalf("accept = %d, %v", got, err)
	}
	if _, err := s.AcceptInvite(ctx, hash("t1"), bo); err != nil {
		t.Fatalf("accept twice: %v", err)
	}

	if err := s.RevokeInvite(ctx, g, ana, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvite(ctx, hash("t1"), newUser(t, s, "cy")); !errors.Is(err, ErrInviteRevoked) {
		t.Fatalf("revoked err = %v", err)
	}

	if _, err := s.CreateInvite(ctx, g, ana, hash("t2"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if invs, _ := s.ActiveInvites(ctx, g); len(invs) != 1 {
		t.Fatalf("active invites = %v", invs)
	}
	c.t = c.t.Add(2 * time.Hour)
	if _, err := s.AcceptInvite(ctx, hash("t2"), newUser(t, s, "dee")); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("expired err = %v", err)
	}
	if invs, _ := s.ActiveInvites(ctx, g); len(invs) != 0 {
		t.Fatalf("active invites after expiry = %v", invs)
	}

	// A former member can rejoin.
	if err := s.LeaveGroup(ctx, g, bo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, g, ana, hash("t3"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvite(ctx, hash("t3"), bo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Membership(ctx, g, bo); err != nil {
		t.Fatalf("rejoined membership: %v", err)
	}
}

func TestDeleteAccount(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ana, bo, cy := newUser(t, s, "Ana Secret"), newUser(t, s, "bo"), newUser(t, s, "cy")
	g := groupWith(t, s, ana, bo, cy)
	if _, err := s.CreateExpense(ctx, g, ana, equal(ana, 300, ana, bo, cy)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, hash("sess"), ana, s.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount(ctx, ana); err != nil {
		t.Fatal(err)
	}

	u, _ := s.User(ctx, ana)
	if u.DisplayName != FormerMemberName(ana) || !u.Deleted {
		t.Fatalf("deleted user = %+v", u)
	}
	if _, err := s.SessionUser(ctx, hash("sess")); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived account deletion")
	}
	if ids, _ := s.Identities(ctx, ana); len(ids) != 0 {
		t.Fatalf("identities survived: %v", ids)
	}
	// Ownership passed to the longest-standing member (bo joined before cy).
	if m, _ := s.Membership(ctx, g, bo); m.Role != RoleOwner {
		t.Fatalf("bo role = %s", m.Role)
	}
	// Balances still add up.
	b := balances(t, s, g)
	if b[ana]+b[bo]+b[cy] != 0 || b[ana] != 200 {
		t.Fatalf("balances = %v", b)
	}
	// Logging in again with the same provider account makes a fresh user.
	again, _ := s.LoginIdentity(ctx, "github", "Ana Secret-id", "Ana Secret")
	if again.ID == ana {
		t.Fatal("deleted account was reused")
	}
	// The old name appears nowhere in the audit log.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE before_json LIKE '%Secret%' OR after_json LIKE '%Secret%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("audit rows mentioning old name: %d, %v", n, err)
	}
}

func TestSettings(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if err := s.UpdateSettings(ctx, map[string]string{"a": "1", "b": "2"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSettings(ctx, map[string]string{"a": "3"}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Settings(ctx)
	if err != nil || len(got) != 1 || got["a"] != "3" {
		t.Fatalf("settings = %v, %v", got, err)
	}
}

func TestClaimAdmin(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ana, bo := newUser(t, s, "ana"), newUser(t, s, "bo")
	if has, _ := s.HasAdmin(ctx); has {
		t.Fatal("admin before claim")
	}
	if ok, err := s.ClaimAdmin(ctx, ana); !ok || err != nil {
		t.Fatalf("ana claim = %v, %v", ok, err)
	}
	if ok, _ := s.ClaimAdmin(ctx, bo); ok {
		t.Fatal("second admin claim succeeded")
	}
	if u, _ := s.User(ctx, ana); !u.IsAdmin {
		t.Fatal("ana not admin")
	}
	// Deleting the admin's account frees the role for a new claim.
	if err := s.DeleteAccount(ctx, ana); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.HasAdmin(ctx); has {
		t.Fatal("deleted user still counts as admin")
	}
	if ok, _ := s.ClaimAdmin(ctx, bo); !ok {
		t.Fatal("bo couldn't claim after admin was deleted")
	}
}
