package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/espm1000/opentripsplit/internal/ledger"
	"github.com/espm1000/opentripsplit/internal/store"
)

// describe turns an audit entry into a sentence. Audit payloads hold user
// IDs only; names are looked up now, so a deleted account shows as
// "Former member #N".
func describe(d *groupData, e store.AuditEntry) historyRow {
	row := historyRow{AuditEntry: e, Actor: d.Name(e.ActorID)}
	switch e.Entity {
	case "expense":
		var before, after *store.ExpenseAudit
		decode(e.Before, &before)
		decode(e.After, &after)
		verb := map[string]string{"create": "added", "update": "edited", "delete": "deleted"}[e.Action]
		row.Summary = fmt.Sprintf("%s expense #%d", verb, e.EntityID)
		if before != nil && after != nil {
			row.Details = []string{"Before: " + expenseText(d, before), "After: " + expenseText(d, after)}
		} else if after != nil {
			row.Details = []string{expenseText(d, after)}
		} else if before != nil {
			row.Details = []string{expenseText(d, before)}
		}
	case "settlement":
		var st *store.SettlementAudit
		decode(e.After, &st)
		if st == nil {
			decode(e.Before, &st)
		}
		verb := map[string]string{"create": "recorded", "delete": "deleted"}[e.Action]
		row.Summary = fmt.Sprintf("%s payment #%d", verb, e.EntityID)
		if st != nil {
			row.Details = []string{fmt.Sprintf("%s paid %s %s", d.Name(st.From), d.Name(st.To), d.Money(st.Amount))}
		}
	case "member":
		who := d.Name(e.EntityID)
		row.Summary = map[string]string{
			"join":            "joined the group",
			"leave":           "left the group",
			"remove":          "removed " + who,
			"transfer":        "made " + who + " the owner",
			"account_deleted": "deleted their account",
		}[e.Action]
	case "group":
		var before, after *store.GroupAudit
		decode(e.Before, &before)
		decode(e.After, &after)
		switch e.Action {
		case "create":
			row.Summary = "created the group"
			if after != nil {
				row.Details = []string{fmt.Sprintf("%q in %s", after.Name, after.Currency)}
			}
		case "rename":
			row.Summary = "renamed the group"
			if before != nil && after != nil {
				row.Details = []string{fmt.Sprintf("%q → %q", before.Name, after.Name)}
			}
		case "archive":
			row.Summary = "archived the group"
		case "unarchive":
			row.Summary = "unarchived the group"
		}
	case "invite":
		switch e.Action {
		case "create":
			row.Summary = fmt.Sprintf("created invite link #%d", e.EntityID)
			var inv *store.InviteAudit
			decode(e.After, &inv)
			if inv != nil {
				row.Details = []string{"Expires " + time.Unix(inv.ExpiresAt, 0).UTC().Format("2 Jan 2006 15:04 MST")}
			}
		case "revoke":
			row.Summary = fmt.Sprintf("revoked invite link #%d", e.EntityID)
		}
	}
	if row.Summary == "" {
		row.Summary = e.Action + " " + e.Entity
	}
	return row
}

func decode[T any](raw json.RawMessage, dst **T) {
	if len(raw) == 0 {
		return
	}
	var v T
	if json.Unmarshal(raw, &v) == nil {
		*dst = &v
	}
}

func expenseText(d *groupData, a *store.ExpenseAudit) string {
	var parts []string
	for _, sh := range a.Shares {
		p := d.Name(sh.UserID) + " " + d.Money(sh.Amount)
		switch a.SplitType {
		case ledger.SplitPercent:
			p += " (" + ledger.FormatAmount(sh.Input, 2) + "%)"
		case ledger.SplitShares:
			p += fmt.Sprintf(" (%d shares)", sh.Input)
		}
		parts = append(parts, p)
	}
	return fmt.Sprintf("%q, %s, %s paid %s, split %s: %s",
		a.Description, a.SpentOn, d.Name(a.PayerID), d.Money(a.Amount), a.SplitType, strings.Join(parts, ", "))
}
