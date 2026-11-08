-- All timestamps are UTC unix seconds. Amounts are integer minor units.

CREATE TABLE users (
    id           INTEGER PRIMARY KEY,
    display_name TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    deleted_at   INTEGER
);

CREATE TABLE identities (
    provider         TEXT    NOT NULL,
    provider_user_id TEXT    NOT NULL,
    user_id          INTEGER NOT NULL REFERENCES users(id),
    created_at       INTEGER NOT NULL,
    PRIMARY KEY (provider, provider_user_id)
);
CREATE INDEX identities_user ON identities(user_id);

CREATE TABLE sessions (
    token_hash BLOB    PRIMARY KEY, -- SHA-256 of the cookie token
    user_id    INTEGER NOT NULL REFERENCES users(id),
    expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE groups (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL,
    currency    TEXT    NOT NULL,
    created_by  INTEGER NOT NULL REFERENCES users(id),
    created_at  INTEGER NOT NULL,
    archived_at INTEGER
);

CREATE TABLE group_members (
    group_id  INTEGER NOT NULL REFERENCES groups(id),
    user_id   INTEGER NOT NULL REFERENCES users(id),
    role      TEXT    NOT NULL CHECK (role IN ('owner', 'member')),
    joined_at INTEGER NOT NULL,
    left_at   INTEGER,
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX group_members_user ON group_members(user_id);

CREATE TABLE invites (
    id         INTEGER PRIMARY KEY,
    token_hash BLOB    NOT NULL UNIQUE, -- SHA-256 of the link token
    group_id   INTEGER NOT NULL REFERENCES groups(id),
    created_by INTEGER NOT NULL REFERENCES users(id),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked_at INTEGER
);
CREATE INDEX invites_group ON invites(group_id);

CREATE TABLE expenses (
    id           INTEGER PRIMARY KEY,
    group_id     INTEGER NOT NULL REFERENCES groups(id),
    payer_id     INTEGER NOT NULL REFERENCES users(id),
    description  TEXT    NOT NULL,
    amount_minor INTEGER NOT NULL CHECK (amount_minor > 0),
    split_type   TEXT    NOT NULL CHECK (split_type IN ('equal', 'exact', 'percent', 'shares')),
    spent_on     TEXT    NOT NULL, -- calendar date, YYYY-MM-DD
    created_by   INTEGER NOT NULL REFERENCES users(id),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    deleted_at   INTEGER
);
CREATE INDEX expenses_group ON expenses(group_id);

CREATE TABLE expense_shares (
    expense_id   INTEGER NOT NULL REFERENCES expenses(id),
    user_id      INTEGER NOT NULL REFERENCES users(id),
    input_value  INTEGER NOT NULL, -- exact: minor units; percent: basis points; shares: weight; equal: 0
    amount_minor INTEGER NOT NULL,
    PRIMARY KEY (expense_id, user_id)
);

CREATE TABLE settlements (
    id           INTEGER PRIMARY KEY,
    group_id     INTEGER NOT NULL REFERENCES groups(id),
    from_user    INTEGER NOT NULL REFERENCES users(id),
    to_user      INTEGER NOT NULL REFERENCES users(id),
    amount_minor INTEGER NOT NULL CHECK (amount_minor > 0),
    created_by   INTEGER NOT NULL REFERENCES users(id),
    created_at   INTEGER NOT NULL,
    deleted_at   INTEGER,
    CHECK (from_user <> to_user)
);
CREATE INDEX settlements_group ON settlements(group_id);

-- before_json/after_json hold user IDs only, never names.
CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY,
    group_id    INTEGER NOT NULL REFERENCES groups(id),
    actor_id    INTEGER NOT NULL REFERENCES users(id),
    action      TEXT    NOT NULL,
    entity      TEXT    NOT NULL,
    entity_id   INTEGER NOT NULL,
    before_json TEXT,
    after_json  TEXT,
    at          INTEGER NOT NULL
);
CREATE INDEX audit_log_group ON audit_log(group_id, id);
