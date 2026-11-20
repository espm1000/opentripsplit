package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/espm1000/opentripsplit/internal/ledger"
)

// Member roles.
const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// Group is a named trip with one currency.
type Group struct {
	ID         int64
	Name       string
	Currency   string
	CreatedAt  time.Time
	ArchivedAt time.Time // zero if active
}

// Archived reports whether the group is read-only.
func (g Group) Archived() bool { return !g.ArchivedAt.IsZero() }

// Decimals returns the number of minor-unit digits of the group currency.
func (g Group) Decimals() int {
	c, _ := ledger.LookupCurrency(g.Currency)
	return c.Decimals
}

// GroupSummary is a group as seen in a user's group list.
type GroupSummary struct {
	Group
	Role string
}

// Member is anyone who has ever joined a group. Former members stay listed
// because past expenses refer to them.
type Member struct {
	UserID   int64
	Name     string
	Role     string
	JoinedAt time.Time
	Active   bool
}

// Invite is a join link. Only the hash of its token is stored, so the link
// itself is shown once, at creation.
type Invite struct {
	ID        int64
	GroupID   int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateGroup creates a group owned by userID.
func (s *Store) CreateGroup(ctx context.Context, userID int64, name, currency string) (int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, err
	}
	if _, ok := ledger.LookupCurrency(currency); !ok {
		return 0, userErr("unknown currency %q", currency)
	}
	var id int64
	err = s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		res, err := tx.ExecContext(ctx, `INSERT INTO groups (name, currency, created_by, created_at) VALUES (?, ?, ?, ?)`,
			name, currency, userID, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members (group_id, user_id, role, joined_at) VALUES (?, ?, 'owner', ?)`,
			id, userID, now); err != nil {
			return err
		}
		return s.audit(ctx, tx, id, userID, "create", "group", id, nil, GroupAudit{Name: name, Currency: currency})
	})
	return id, err
}

// GroupsForUser lists the groups userID is an active member of, newest first.
func (s *Store) GroupsForUser(ctx context.Context, userID int64) ([]GroupSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.id, g.name, g.currency, g.created_at, g.archived_at, m.role
		FROM groups g JOIN group_members m ON m.group_id = g.id
		WHERE m.user_id = ? AND m.left_at IS NULL
		ORDER BY g.created_at DESC, g.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupSummary
	for rows.Next() {
		var g GroupSummary
		var created int64
		var archived sql.NullInt64
		if err := rows.Scan(&g.ID, &g.Name, &g.Currency, &created, &archived, &g.Role); err != nil {
			return nil, err
		}
		g.CreatedAt, g.ArchivedAt = fromUnix(created), fromNullUnix(archived)
		out = append(out, g)
	}
	return out, rows.Err()
}

// Group returns a group by ID.
func (s *Store) Group(ctx context.Context, id int64) (Group, error) {
	return loadGroup(ctx, s.db, id)
}

func loadGroup(ctx context.Context, q querier, id int64) (Group, error) {
	g := Group{ID: id}
	var created int64
	var archived sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT name, currency, created_at, archived_at FROM groups WHERE id = ?`, id).
		Scan(&g.Name, &g.Currency, &created, &archived)
	g.CreatedAt, g.ArchivedAt = fromUnix(created), fromNullUnix(archived)
	return g, notFound(err)
}

// Membership returns userID's active membership in a group, or ErrNotFound.
func (s *Store) Membership(ctx context.Context, groupID, userID int64) (Member, error) {
	return activeMember(ctx, s.db, groupID, userID)
}

func activeMember(ctx context.Context, q querier, groupID, userID int64) (Member, error) {
	m := Member{UserID: userID, Active: true}
	var joined int64
	err := q.QueryRowContext(ctx, `
		SELECT u.display_name, m.role, m.joined_at FROM group_members m JOIN users u ON u.id = m.user_id
		WHERE m.group_id = ? AND m.user_id = ? AND m.left_at IS NULL`, groupID, userID).Scan(&m.Name, &m.Role, &joined)
	m.JoinedAt = fromUnix(joined)
	return m, notFound(err)
}

// Members lists everyone who has ever joined the group: active members
// first (owner, then by join time), then former members.
func (s *Store) Members(ctx context.Context, groupID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.user_id, u.display_name, m.role, m.joined_at, m.left_at IS NULL
		FROM group_members m JOIN users u ON u.id = m.user_id
		WHERE m.group_id = ?
		ORDER BY m.left_at IS NOT NULL, m.role = 'member', m.joined_at, m.user_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var joined int64
		if err := rows.Scan(&m.UserID, &m.Name, &m.Role, &joined, &m.Active); err != nil {
			return nil, err
		}
		m.JoinedAt = fromUnix(joined)
		out = append(out, m)
	}
	return out, rows.Err()
}

// writableAs checks, inside a transaction, that the group exists and is not
// archived and that actorID is an active member. If ownerOnly, the actor
// must also be the owner.
func writableAs(ctx context.Context, tx *sql.Tx, groupID, actorID int64, ownerOnly bool) (Group, Member, error) {
	g, err := loadGroup(ctx, tx, groupID)
	if err != nil {
		return g, Member{}, err
	}
	m, err := activeMember(ctx, tx, groupID, actorID)
	if err != nil {
		return g, m, err
	}
	if g.Archived() {
		return g, m, ErrArchived
	}
	if ownerOnly && m.Role != RoleOwner {
		return g, m, ErrForbidden
	}
	return g, m, nil
}

// RenameGroup changes the group name. Owner only.
func (s *Store) RenameGroup(ctx context.Context, groupID, actorID int64, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		g, _, err := writableAs(ctx, tx, groupID, actorID, true)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE groups SET name = ? WHERE id = ?`, name, groupID); err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, "rename", "group", groupID,
			GroupAudit{Name: g.Name, Currency: g.Currency}, GroupAudit{Name: name, Currency: g.Currency})
	})
}

// SetArchived archives or unarchives a group. Owner only.
func (s *Store) SetArchived(ctx context.Context, groupID, actorID int64, archived bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		g, err := loadGroup(ctx, tx, groupID)
		if err != nil {
			return err
		}
		m, err := activeMember(ctx, tx, groupID, actorID)
		if err != nil {
			return err
		}
		if m.Role != RoleOwner {
			return ErrForbidden
		}
		if g.Archived() == archived {
			return nil
		}
		var at any
		action := "unarchive"
		if archived {
			at, action = s.now(), "archive"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE groups SET archived_at = ? WHERE id = ?`, at, groupID); err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, action, "group", groupID, nil, nil)
	})
}

// LeaveGroup removes the actor from the group. Their balance must be zero
// and the owner must hand over ownership first.
func (s *Store) LeaveGroup(ctx context.Context, groupID, actorID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, m, err := writableAs(ctx, tx, groupID, actorID, false)
		if err != nil {
			return err
		}
		if m.Role == RoleOwner {
			return ErrOwnerMustStay
		}
		return s.removeMember(ctx, tx, groupID, actorID, actorID, "leave")
	})
}

// RemoveMember removes another member. Owner only; the member's balance
// must be zero.
func (s *Store) RemoveMember(ctx context.Context, groupID, actorID, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, _, err := writableAs(ctx, tx, groupID, actorID, true); err != nil {
			return err
		}
		if userID == actorID {
			return ErrOwnerMustStay
		}
		if _, err := activeMember(ctx, tx, groupID, userID); err != nil {
			return err
		}
		return s.removeMember(ctx, tx, groupID, actorID, userID, "remove")
	})
}

func (s *Store) removeMember(ctx context.Context, tx *sql.Tx, groupID, actorID, userID int64, action string) error {
	bal, err := groupBalances(ctx, tx, groupID)
	if err != nil {
		return err
	}
	if bal[userID] != 0 {
		return ErrNonZeroBalance
	}
	if _, err := tx.ExecContext(ctx, `UPDATE group_members SET left_at = ? WHERE group_id = ? AND user_id = ?`,
		s.now(), groupID, userID); err != nil {
		return err
	}
	return s.audit(ctx, tx, groupID, actorID, action, "member", userID, MemberAudit{UserID: userID, Role: RoleMember}, nil)
}

// TransferOwnership makes userID the owner and the actor a regular member.
func (s *Store) TransferOwnership(ctx context.Context, groupID, actorID, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, _, err := writableAs(ctx, tx, groupID, actorID, true); err != nil {
			return err
		}
		if userID == actorID {
			return nil
		}
		if _, err := activeMember(ctx, tx, groupID, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE group_members SET role = CASE user_id WHEN ? THEN 'owner' ELSE 'member' END
			WHERE group_id = ? AND user_id IN (?, ?)`, userID, groupID, userID, actorID); err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, "transfer", "member", userID,
			MemberAudit{UserID: userID, Role: RoleMember}, MemberAudit{UserID: userID, Role: RoleOwner})
	})
}

// CreateInvite stores a join link valid for ttl. Owner only.
func (s *Store) CreateInvite(ctx context.Context, groupID, actorID int64, tokenHash []byte, ttl time.Duration) (Invite, error) {
	inv := Invite{GroupID: groupID}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, _, err := writableAs(ctx, tx, groupID, actorID, true); err != nil {
			return err
		}
		now := s.Now().UTC().Truncate(time.Second)
		inv.CreatedAt, inv.ExpiresAt = now, now.Add(ttl)
		res, err := tx.ExecContext(ctx, `INSERT INTO invites (token_hash, group_id, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			tokenHash, groupID, actorID, now.Unix(), inv.ExpiresAt.Unix())
		if err != nil {
			return err
		}
		if inv.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		return s.audit(ctx, tx, groupID, actorID, "create", "invite", inv.ID, nil, InviteAudit{ExpiresAt: inv.ExpiresAt.Unix()})
	})
	return inv, err
}

// ActiveInvites lists unexpired, unrevoked invites for a group.
func (s *Store) ActiveInvites(ctx context.Context, groupID int64) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, expires_at FROM invites
		WHERE group_id = ? AND revoked_at IS NULL AND expires_at > ?
		ORDER BY id DESC`, groupID, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		inv := Invite{GroupID: groupID}
		var created, expires int64
		if err := rows.Scan(&inv.ID, &created, &expires); err != nil {
			return nil, err
		}
		inv.CreatedAt, inv.ExpiresAt = fromUnix(created), fromUnix(expires)
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvite disables an invite link. Owner only.
func (s *Store) RevokeInvite(ctx context.Context, groupID, actorID, inviteID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, _, err := writableAs(ctx, tx, groupID, actorID, true); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE invites SET revoked_at = ? WHERE id = ? AND group_id = ? AND revoked_at IS NULL`,
			s.now(), inviteID, groupID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, groupID, actorID, "revoke", "invite", inviteID, nil, nil)
	})
}

// InviteGroup returns the group an invite token leads to, or an error if the
// invite is unknown, expired, revoked, or its group is archived.
func (s *Store) InviteGroup(ctx context.Context, tokenHash []byte) (Group, error) {
	return validInvite(ctx, s.db, tokenHash, s.now())
}

func validInvite(ctx context.Context, q querier, tokenHash []byte, now int64) (Group, error) {
	var groupID, expires int64
	var revoked sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT group_id, expires_at, revoked_at FROM invites WHERE token_hash = ?`, tokenHash).
		Scan(&groupID, &expires, &revoked)
	if err != nil {
		return Group{}, notFound(err)
	}
	switch {
	case revoked.Valid:
		return Group{}, ErrInviteRevoked
	case expires <= now:
		return Group{}, ErrInviteExpired
	}
	g, err := loadGroup(ctx, q, groupID)
	if err == nil && g.Archived() {
		err = ErrArchived
	}
	return g, err
}

// AcceptInvite adds userID to the invite's group (or re-adds a former
// member) and returns the group ID. Joining twice is a no-op.
func (s *Store) AcceptInvite(ctx context.Context, tokenHash []byte, userID int64) (int64, error) {
	var groupID int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		g, err := validInvite(ctx, tx, tokenHash, s.now())
		if err != nil {
			return err
		}
		groupID = g.ID
		_, err = activeMember(ctx, tx, g.ID, userID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO group_members (group_id, user_id, role, joined_at) VALUES (?, ?, 'member', ?)
			ON CONFLICT (group_id, user_id) DO UPDATE SET role = 'member', joined_at = excluded.joined_at, left_at = NULL`,
			g.ID, userID, s.now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, g.ID, userID, "join", "member", userID, nil, MemberAudit{UserID: userID, Role: RoleMember})
	})
	return groupID, err
}
