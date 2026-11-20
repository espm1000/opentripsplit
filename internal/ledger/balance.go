package ledger

import "slices"

// Expense is the ledger view of an expense: who paid, and who owes what.
type Expense struct {
	PayerID  int64
	Amount   int64
	Portions []Portion
}

// Settlement is a recorded payment from one member to another.
type Settlement struct {
	From, To int64
	Amount   int64
}

// Payment is a suggested settle-up transfer.
type Payment struct {
	From, To int64
	Amount   int64
}

// Balances returns each user's balance: paid − owed + settlements sent −
// settlements received. Positive means the group owes them. The values
// always sum to zero. Users with a zero balance may be absent.
func Balances(expenses []Expense, settlements []Settlement) map[int64]int64 {
	b := map[int64]int64{}
	for _, e := range expenses {
		b[e.PayerID] += e.Amount
		for _, p := range e.Portions {
			b[p.UserID] -= p.Amount
		}
	}
	for _, s := range settlements {
		b[s.From] += s.Amount
		b[s.To] -= s.Amount
	}
	return b
}

// SettleUp suggests payments that bring every balance to zero by repeatedly
// matching the largest creditor with the largest debtor. It needs at most
// n−1 payments for n non-zero balances. Ties are broken by user ID so the
// result is deterministic.
func SettleUp(balances map[int64]int64) []Payment {
	type entry struct{ id, amt int64 }
	var cred, debt []entry
	for id, v := range balances {
		switch {
		case v > 0:
			cred = append(cred, entry{id, v})
		case v < 0:
			debt = append(debt, entry{id, -v})
		}
	}
	byAmount := func(a, b entry) int {
		if a.amt != b.amt {
			return cmpInt(b.amt, a.amt)
		}
		return cmpInt(a.id, b.id)
	}

	var out []Payment
	for len(cred) > 0 && len(debt) > 0 {
		slices.SortFunc(cred, byAmount)
		slices.SortFunc(debt, byAmount)
		c, d := &cred[0], &debt[0]
		amt := min(c.amt, d.amt)
		out = append(out, Payment{From: d.id, To: c.id, Amount: amt})
		c.amt -= amt
		d.amt -= amt
		if c.amt == 0 {
			cred = cred[1:]
		}
		if d.amt == 0 {
			debt = debt[1:]
		}
	}
	return out
}
