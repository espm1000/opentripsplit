package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxNameLen caps display and group names, in characters.
const MaxNameLen = 60

// User is an account. Deleted users keep their row (renamed to
// "Former member #N") so group balances still add up.
type User struct {
	ID          int64
	DisplayName string
	Deleted     bool
	IsAdmin     bool // manages instance settings in the UI
}

// Identity is a login provider account linked to a user.
type Identity struct {
	Provider       string
	ProviderUserID string
}

// FormerMemberName is the display name given to a deleted account. The user
// ID makes it unique per instance.
func FormerMemberName(userID int64) string { return fmt.Sprintf("Former member #%d", userID) }

func cleanName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", userErr("name can't be empty")
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return "", userErr("name can be at most %d characters", MaxNameLen)
	}
	return name, nil
}

// LoginIdentity returns the user linked to the provider identity, creating a
// new user named displayName if there is none.
func (s *Store) LoginIdentity(ctx context.Context, provider, providerUserID, displayName string) (User, error) {
	var u User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT u.id, u.display_name, u.is_admin FROM identities i JOIN users u ON u.id = i.user_id
			WHERE i.provider = ? AND i.provider_user_id = ?`, provider, providerUserID).Scan(&u.ID, &u.DisplayName, &u.IsAdmin)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		name, err := cleanName(displayName)
		if err != nil {
			name = provider + " user"
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO users (display_name, created_at) VALUES (?, ?)`, name, s.now())
		if err != nil {
			return err
		}
		if u.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		u.DisplayName = name
		_, err = tx.ExecContext(ctx, `INSERT INTO identities (provider, provider_user_id, user_id, created_at) VALUES (?, ?, ?, ?)`,
			provider, providerUserID, u.ID, s.now())
		return err
	})
	return u, err
}

// LinkIdentity attaches another provider identity to userID. Linking an
// identity the user already has is a no-op.
func (s *Store) LinkIdentity(ctx context.Context, userID int64, provider, providerUserID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var owner int64
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM identities WHERE provider = ? AND provider_user_id = ?`,
			provider, providerUserID).Scan(&owner)
		switch {
		case err == nil && owner == userID:
			return nil
		case err == nil:
			return ErrIdentityTaken
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO identities (provider, provider_user_id, user_id, created_at) VALUES (?, ?, ?, ?)`,
			provider, providerUserID, userID, s.now())
		return err
	})
}

// Identities lists the providers linked to a user.
func (s *Store) Identities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, provider_user_id FROM identities WHERE user_id = ? ORDER BY provider`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.Provider, &i.ProviderUserID); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// User returns a user by ID.
func (s *Store) User(ctx context.Context, id int64) (User, error) {
	u := User{ID: id}
	var deleted sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT display_name, deleted_at, is_admin FROM users WHERE id = ?`, id).Scan(&u.DisplayName, &deleted, &u.IsAdmin)
	u.Deleted = deleted.Valid
	return u, notFound(err)
}

// SetDisplayName renames a user.
func (s *Store) SetDisplayName(ctx context.Context, userID int64, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE users SET display_name = ? WHERE id = ? AND deleted_at IS NULL`, name, userID)
	return err
}

// CreateSession stores a session keyed by the SHA-256 of its cookie token.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		tokenHash, userID, expires.UTC().Unix())
	return err
}

// SessionUser returns the user for an unexpired session.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `
		SELECT u.id, u.display_name, u.is_admin FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ? AND u.deleted_at IS NULL`, tokenHash, s.now()).Scan(&u.ID, &u.DisplayName, &u.IsAdmin)
	return u, notFound(err)
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// PurgeExpiredSessions deletes expired sessions and returns how many.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, s.now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAccount removes the user's identities and sessions and renames them
// to "Former member #N". They leave every group; where they were owner,
// ownership passes to the longest-standing remaining member.
func (s *Store) DeleteAccount(ctx context.Context, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		res, err := tx.ExecContext(ctx, `UPDATE users SET display_name = ?, deleted_at = ?, is_admin = 0 WHERE id = ? AND deleted_at IS NULL`,
			FormerMemberName(userID), now, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE user_id = ?`, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return err
		}

		ms, err := activeMemberships(ctx, tx, userID)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if m.role == RoleOwner {
				var heir int64
				err := tx.QueryRowContext(ctx, `
					SELECT user_id FROM group_members
					WHERE group_id = ? AND user_id <> ? AND left_at IS NULL
					ORDER BY joined_at, user_id LIMIT 1`, m.groupID, userID).Scan(&heir)
				switch {
				case err == nil:
					if _, err := tx.ExecContext(ctx, `UPDATE group_members SET role = 'owner' WHERE group_id = ? AND user_id = ?`, m.groupID, heir); err != nil {
						return err
					}
					if err := s.audit(ctx, tx, m.groupID, userID, "transfer", "member", heir, nil, MemberAudit{UserID: heir, Role: RoleOwner}); err != nil {
						return err
					}
				case !errors.Is(err, sql.ErrNoRows):
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE group_members SET role = 'member', left_at = ? WHERE group_id = ? AND user_id = ?`,
				now, m.groupID, userID); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, m.groupID, userID, "account_deleted", "member", userID, MemberAudit{UserID: userID, Role: m.role}, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

type membership struct {
	groupID int64
	role    string
}

func activeMemberships(ctx context.Context, q querier, userID int64) ([]membership, error) {
	rows, err := q.QueryContext(ctx, `SELECT group_id, role FROM group_members WHERE user_id = ? AND left_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ms []membership
	for rows.Next() {
		var m membership
		if err := rows.Scan(&m.groupID, &m.role); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	return ms, rows.Err()
}
