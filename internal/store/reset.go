package store

import (
	"context"
	"database/sql"
	"fmt"
)

// resetTables are emptied by ResetData, children before parents so the
// foreign keys hold at every step.
var resetTables = []string{
	"audit_log", "expense_shares", "expenses", "settlements",
	"invites", "group_members", "groups",
}

// ResetData deletes every group and every user except keepUserID, who must
// be the instance admin. The admin's logins and sessions are kept, as are
// instance settings, so the admin stays logged in and login providers keep
// working. Afterwards the database file is compacted so the deleted rows
// are gone from disk, not just unlinked.
func (s *Store) ResetData(ctx context.Context, keepUserID int64) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var isAdmin bool
		err := tx.QueryRowContext(ctx, `SELECT is_admin FROM users WHERE id = ? AND deleted_at IS NULL`, keepUserID).Scan(&isAdmin)
		if err != nil {
			return notFound(err)
		}
		if !isAdmin {
			return ErrForbidden
		}
		for _, t := range resetTables {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil { // #nosec G202 -- t is from the fixed list above
				return fmt.Errorf("clear %s: %w", t, err)
			}
		}
		for _, q := range []string{
			`DELETE FROM sessions WHERE user_id <> ?`,
			`DELETE FROM identities WHERE user_id <> ?`,
			`DELETE FROM users WHERE id <> ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, keepUserID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// VACUUM can't run inside a transaction. The data is already reset if
	// it fails; the rows just linger in free pages until the next VACUUM.
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}
