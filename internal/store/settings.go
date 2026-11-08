package store

import (
	"context"
	"database/sql"
)

// Settings returns every stored instance setting.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// UpdateSettings stores the set values and deletes the del keys, atomically.
func (s *Store) UpdateSettings(ctx context.Context, set map[string]string, del []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, k := range del {
			if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, k); err != nil {
				return err
			}
		}
		for k, v := range set {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
				ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				k, v, s.now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// HasAdmin reports whether the instance has an admin.
func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_admin = 1 AND deleted_at IS NULL`).Scan(&n)
	return n > 0, err
}

// ClaimAdmin makes userID the admin if there is none yet, and reports
// whether it did.
func (s *Store) ClaimAdmin(ctx context.Context, userID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users SET is_admin = 1
		WHERE id = ? AND deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM users WHERE is_admin = 1 AND deleted_at IS NULL)`, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
