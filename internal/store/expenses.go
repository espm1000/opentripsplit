package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/espm1000/opentripsplit/internal/ledger"
)

// MaxDescriptionLen caps an expense description, in characters.
const MaxDescriptionLen = 200

// Expense is a payment by one member, split between participants.
type Expense struct {
	ID          int64
	GroupID     int64
	PayerID     int64
	Description string
	Amount      int64
	SplitType   ledger.SplitType
	SpentOn     string // YYYY-MM-DD
	CreatedBy   int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Shares      []ExpenseShare // sorted by user ID
}

// Participants returns the payer and everyone with a share.
func (e Expense) Participants() []int64 {
	ids := []int64{e.PayerID}
	for _, sh := range e.Shares {
		ids = append(ids, sh.UserID)
	}
	return ids
}

// ExpenseShare is one participant's raw input and computed portion.
type ExpenseShare struct {
	UserID int64
	Input  int64
	Amount int64
}

// ExpenseInput is what a user submits to create or edit an expense.
type ExpenseInput struct {
	PayerID     int64
	Description string
	Amount      int64
	SplitType   ledger.SplitType
	SpentOn     string
	Shares      []ledger.Share
}

// Settlement is a recorded payment between two members.
type Settlement struct {
	ID        int64
	GroupID   int64
	From, To  int64
	Amount    int64
	CreatedBy int64
	CreatedAt time.Time
}

func (in *ExpenseInput) clean() error {
	in.Description = strings.Join(strings.Fields(in.Description), " ")
	if in.Description == "" {
		return userErr("description can't be empty")
	}
	if utf8.RuneCountInString(in.Description) > MaxDescriptionLen {
		return userErr("description can be at most %d characters", MaxDescriptionLen)
	}
	if _, err := time.Parse(time.DateOnly, in.SpentOn); err != nil {
		return userErr("date must look like 2026-07-14")
	}
	if !in.SplitType.Valid() {
		return userErr("unknown split type")
	}
	return nil
}

// checkParticipants verifies the payer and every participant are active
// members of the group.
func checkParticipants(ctx context.Context, tx *sql.Tx, groupID int64, in ExpenseInput) error {
	ids := []int64{in.PayerID}
	for _, sh := range in.Shares {
		ids = append(ids, sh.UserID)
	}
	for _, id := range ids {
		if _, err := activeMember(ctx, tx, groupID, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				return userErr("everyone in an expense must be a current member of the group")
			}
			return err
		}
	}
	return nil
}

// writeShares computes the split (rotating the rounding tiebreak by expense
// ID) and replaces the expense's share rows.
func writeShares(ctx context.Context, tx *sql.Tx, expenseID int64, in ExpenseInput) ([]ExpenseShare, error) {
	portions, err := ledger.Split(in.Amount, in.SplitType, in.Shares, expenseID)
	if err != nil {
		return nil, &UserError{err.Error()}
	}
	inputs := map[int64]int64{}
	for _, sh := range in.Shares {
		if in.SplitType != ledger.SplitEqual {
			inputs[sh.UserID] = sh.Input
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM expense_shares WHERE expense_id = ?`, expenseID); err != nil {
		return nil, err
	}
	out := make([]ExpenseShare, len(portions))
	for i, p := range portions {
		out[i] = ExpenseShare{UserID: p.UserID, Input: inputs[p.UserID], Amount: p.Amount}
		if _, err := tx.ExecContext(ctx, `INSERT INTO expense_shares (expense_id, user_id, input_value, amount_minor) VALUES (?, ?, ?, ?)`,
			expenseID, p.UserID, out[i].Input, p.Amount); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CreateExpense adds an expense. Any active member may add one.
func (s *Store) CreateExpense(ctx context.Context, groupID, actorID int64, in ExpenseInput) (int64, error) {
	if err := in.clean(); err != nil {
		return 0, err
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, _, err := writableAs(ctx, tx, groupID, actorID, false); err != nil {
			return err
		}
		if err := checkParticipants(ctx, tx, groupID, in); err != nil {
			return err
		}
		now := s.now()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO expenses (group_id, payer_id, description, amount_minor, split_type, spent_on, created_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			groupID, in.PayerID, in.Description, in.Amount, string(in.SplitType), in.SpentOn, actorID, now, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		shares, err := writeShares(ctx, tx, id, in)
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, "create", "expense", id, nil, expenseAudit(in, shares))
	})
	return id, err
}

// editableExpense loads an expense for edit/delete and checks the actor is
// its creator or the group owner.
func editableExpense(ctx context.Context, tx *sql.Tx, groupID, expenseID, actorID int64) (Expense, error) {
	_, m, err := writableAs(ctx, tx, groupID, actorID, false)
	if err != nil {
		return Expense{}, err
	}
	e, err := loadExpense(ctx, tx, groupID, expenseID)
	if err != nil {
		return e, err
	}
	if e.CreatedBy != actorID && m.Role != RoleOwner {
		return e, ErrForbidden
	}
	return e, nil
}

// UpdateExpense edits an expense. Creator or owner only.
func (s *Store) UpdateExpense(ctx context.Context, groupID, expenseID, actorID int64, in ExpenseInput) error {
	if err := in.clean(); err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		old, err := editableExpense(ctx, tx, groupID, expenseID, actorID)
		if err != nil {
			return err
		}
		for _, id := range old.Participants() {
			if _, err := activeMember(ctx, tx, groupID, id); errors.Is(err, ErrNotFound) {
				return ErrFormerParticipant
			} else if err != nil {
				return err
			}
		}
		if err := checkParticipants(ctx, tx, groupID, in); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE expenses SET payer_id = ?, description = ?, amount_minor = ?, split_type = ?, spent_on = ?, updated_at = ?
			WHERE id = ?`, in.PayerID, in.Description, in.Amount, string(in.SplitType), in.SpentOn, s.now(), expenseID); err != nil {
			return err
		}
		shares, err := writeShares(ctx, tx, expenseID, in)
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, "update", "expense", expenseID, auditOfExpense(old), expenseAudit(in, shares))
	})
}

// DeleteExpense soft-deletes an expense. Creator or owner only.
func (s *Store) DeleteExpense(ctx context.Context, groupID, expenseID, actorID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		old, err := editableExpense(ctx, tx, groupID, expenseID, actorID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE expenses SET deleted_at = ? WHERE id = ?`, s.now(), expenseID); err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, "delete", "expense", expenseID, auditOfExpense(old), nil)
	})
}

// Expense returns one non-deleted expense in a group.
func (s *Store) Expense(ctx context.Context, groupID, expenseID int64) (Expense, error) {
	return loadExpense(ctx, s.db, groupID, expenseID)
}

func loadExpense(ctx context.Context, q querier, groupID, expenseID int64) (Expense, error) {
	es, err := loadExpenses(ctx, q, groupID, expenseID)
	if err != nil {
		return Expense{}, err
	}
	if len(es) == 0 {
		return Expense{}, ErrNotFound
	}
	return es[0], nil
}

// Expenses lists a group's non-deleted expenses, newest first.
func (s *Store) Expenses(ctx context.Context, groupID int64) ([]Expense, error) {
	return loadExpenses(ctx, s.db, groupID, 0)
}

// loadExpenses loads the group's expenses, or just one if onlyID != 0.
func loadExpenses(ctx context.Context, q querier, groupID, onlyID int64) ([]Expense, error) {
	out, err := loadExpenseRows(ctx, q, groupID, onlyID)
	if err != nil {
		return nil, err
	}
	index := map[int64]int{}
	for i, e := range out {
		index[e.ID] = i
	}
	rows, err := q.QueryContext(ctx, `
		SELECT s.expense_id, s.user_id, s.input_value, s.amount_minor
		FROM expense_shares s JOIN expenses e ON e.id = s.expense_id
		WHERE e.group_id = ? AND e.deleted_at IS NULL AND (? = 0 OR e.id = ?)
		ORDER BY s.expense_id, s.user_id`, groupID, onlyID, onlyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var eid int64
		var sh ExpenseShare
		if err := rows.Scan(&eid, &sh.UserID, &sh.Input, &sh.Amount); err != nil {
			return nil, err
		}
		if i, ok := index[eid]; ok {
			out[i].Shares = append(out[i].Shares, sh)
		}
	}
	return out, rows.Err()
}

func loadExpenseRows(ctx context.Context, q querier, groupID, onlyID int64) ([]Expense, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, payer_id, description, amount_minor, split_type, spent_on, created_by, created_at, updated_at
		FROM expenses
		WHERE group_id = ? AND deleted_at IS NULL AND (? = 0 OR id = ?)
		ORDER BY spent_on DESC, id DESC`, groupID, onlyID, onlyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Expense
	for rows.Next() {
		e := Expense{GroupID: groupID}
		var created, updated int64
		var typ string
		if err := rows.Scan(&e.ID, &e.PayerID, &e.Description, &e.Amount, &typ, &e.SpentOn, &e.CreatedBy, &created, &updated); err != nil {
			return nil, err
		}
		e.SplitType, e.CreatedAt, e.UpdatedAt = ledger.SplitType(typ), fromUnix(created), fromUnix(updated)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CreateSettlement records that from paid to. The actor must be one of the
// two parties, or the group owner (e.g. to settle with a former member).
func (s *Store) CreateSettlement(ctx context.Context, groupID, actorID, from, to, amount int64) (int64, error) {
	if from == to {
		return 0, userErr("pick two different people")
	}
	if amount <= 0 || amount > ledger.MaxAmount {
		return 0, userErr("amount must be greater than zero")
	}
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, m, err := writableAs(ctx, tx, groupID, actorID, false)
		if err != nil {
			return err
		}
		if actorID != from && actorID != to && m.Role != RoleOwner {
			return ErrForbidden
		}
		for _, uid := range []int64{from, to} {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_members WHERE group_id = ? AND user_id = ?`, groupID, uid).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return userErr("both people must be members of the group")
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO settlements (group_id, from_user, to_user, amount_minor, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			groupID, from, to, amount, actorID, s.now())
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, "create", "settlement", id, nil, SettlementAudit{From: from, To: to, Amount: amount})
	})
	return id, err
}

// DeleteSettlement soft-deletes a settlement. Creator or owner only.
func (s *Store) DeleteSettlement(ctx context.Context, groupID, settlementID, actorID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, m, err := writableAs(ctx, tx, groupID, actorID, false)
		if err != nil {
			return err
		}
		var st Settlement
		err = tx.QueryRowContext(ctx, `SELECT from_user, to_user, amount_minor, created_by FROM settlements
			WHERE id = ? AND group_id = ? AND deleted_at IS NULL`, settlementID, groupID).Scan(&st.From, &st.To, &st.Amount, &st.CreatedBy)
		if err != nil {
			return notFound(err)
		}
		if st.CreatedBy != actorID && m.Role != RoleOwner {
			return ErrForbidden
		}
		if _, err := tx.ExecContext(ctx, `UPDATE settlements SET deleted_at = ? WHERE id = ?`, s.now(), settlementID); err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, "delete", "settlement", settlementID,
			SettlementAudit{From: st.From, To: st.To, Amount: st.Amount}, nil)
	})
}

// Settlements lists a group's non-deleted settlements, newest first.
func (s *Store) Settlements(ctx context.Context, groupID int64) ([]Settlement, error) {
	return loadSettlements(ctx, s.db, groupID)
}

func loadSettlements(ctx context.Context, q querier, groupID int64) ([]Settlement, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, from_user, to_user, amount_minor, created_by, created_at FROM settlements
		WHERE group_id = ? AND deleted_at IS NULL ORDER BY created_at DESC, id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Settlement
	for rows.Next() {
		st := Settlement{GroupID: groupID}
		var created int64
		if err := rows.Scan(&st.ID, &st.From, &st.To, &st.Amount, &st.CreatedBy, &created); err != nil {
			return nil, err
		}
		st.CreatedAt = fromUnix(created)
		out = append(out, st)
	}
	return out, rows.Err()
}

// Balances computes per-user balances from loaded expenses and settlements.
func Balances(expenses []Expense, settlements []Settlement) map[int64]int64 {
	le := make([]ledger.Expense, len(expenses))
	for i, e := range expenses {
		le[i] = ledger.Expense{PayerID: e.PayerID, Amount: e.Amount}
		for _, sh := range e.Shares {
			le[i].Portions = append(le[i].Portions, ledger.Portion{UserID: sh.UserID, Amount: sh.Amount})
		}
	}
	ls := make([]ledger.Settlement, len(settlements))
	for i, st := range settlements {
		ls[i] = ledger.Settlement{From: st.From, To: st.To, Amount: st.Amount}
	}
	return ledger.Balances(le, ls)
}

func groupBalances(ctx context.Context, q querier, groupID int64) (map[int64]int64, error) {
	es, err := loadExpenses(ctx, q, groupID, 0)
	if err != nil {
		return nil, err
	}
	ss, err := loadSettlements(ctx, q, groupID)
	if err != nil {
		return nil, err
	}
	return Balances(es, ss), nil
}
