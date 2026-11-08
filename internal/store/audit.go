package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/espm1000/opentripsplit/internal/ledger"
)

// The *Audit types are the JSON payloads stored in audit_log.before_json and
// after_json. They hold user IDs only, never names, so deleting an account
// leaves no old name behind.

// ExpenseAudit is an expense snapshot.
type ExpenseAudit struct {
	PayerID     int64            `json:"payer_id"`
	Description string           `json:"description"`
	Amount      int64            `json:"amount_minor"`
	SplitType   ledger.SplitType `json:"split_type"`
	SpentOn     string           `json:"spent_on"`
	Shares      []ShareAudit     `json:"shares"`
}

// ShareAudit is one participant's input and computed amount.
type ShareAudit struct {
	UserID int64 `json:"user_id"`
	Input  int64 `json:"input_value"`
	Amount int64 `json:"amount_minor"`
}

// SettlementAudit is a settlement snapshot.
type SettlementAudit struct {
	From   int64 `json:"from_user"`
	To     int64 `json:"to_user"`
	Amount int64 `json:"amount_minor"`
}

// MemberAudit records a membership change.
type MemberAudit struct {
	UserID int64  `json:"user_id"`
	Role   string `json:"role"`
}

// GroupAudit is a group snapshot.
type GroupAudit struct {
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

// InviteAudit records an invite's expiry (unix seconds).
type InviteAudit struct {
	ExpiresAt int64 `json:"expires_at"`
}

// AuditEntry is one row of a group's history.
type AuditEntry struct {
	ID       int64
	ActorID  int64
	Action   string
	Entity   string
	EntityID int64
	Before   json.RawMessage // nil if none
	After    json.RawMessage // nil if none
	At       time.Time
}

func expenseAudit(in ExpenseInput, shares []ExpenseShare) ExpenseAudit {
	a := ExpenseAudit{PayerID: in.PayerID, Description: in.Description, Amount: in.Amount, SplitType: in.SplitType, SpentOn: in.SpentOn}
	for _, sh := range shares {
		a.Shares = append(a.Shares, ShareAudit(sh))
	}
	return a
}

func auditOfExpense(e Expense) ExpenseAudit {
	a := ExpenseAudit{PayerID: e.PayerID, Description: e.Description, Amount: e.Amount, SplitType: e.SplitType, SpentOn: e.SpentOn}
	for _, sh := range e.Shares {
		a.Shares = append(a.Shares, ShareAudit(sh))
	}
	return a
}

func (s *Store) audit(ctx context.Context, tx *sql.Tx, groupID, actorID int64, action, entity string, entityID int64, before, after any) error {
	b, err := marshalNullable(before)
	if err != nil {
		return err
	}
	a, err := marshalNullable(after)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_log (group_id, actor_id, action, entity, entity_id, before_json, after_json, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, groupID, actorID, action, entity, entityID, b, a, s.now())
	return err
}

func marshalNullable(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// AuditLog returns a group's history, newest first.
func (s *Store) AuditLog(ctx context.Context, groupID int64) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, actor_id, action, entity, entity_id, before_json, after_json, at
		FROM audit_log WHERE group_id = ? ORDER BY id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var before, after sql.NullString
		var at int64
		if err := rows.Scan(&e.ID, &e.ActorID, &e.Action, &e.Entity, &e.EntityID, &before, &after, &at); err != nil {
			return nil, err
		}
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		e.At = fromUnix(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
