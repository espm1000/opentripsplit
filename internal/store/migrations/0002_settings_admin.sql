-- Instance settings edited in the admin UI (e.g. OAuth client credentials).
-- Environment variables take precedence over these.
CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

-- The instance admin manages settings in the UI. The first person to log in
-- after completing /setup with the setup token becomes admin.
ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0;
