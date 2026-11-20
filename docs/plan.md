# Plan: Group trip expense splitter (Go, open source)

## Context
You want a self-hostable web app where friends log who paid for what on a trip, and the app works out who owes whom when the group settles up. Requirements: accounts, named groups, past groups kept for review, login with no local passwords and minimal personal data, persistent storage, open source under MIT. This plan covers the GitHub setup, license, and the app build.

**Name: OpenTripSplit** (repo/module/binary: `opentripsplit`). Checked 2026-11-08: no GitHub repos with that name, `github.com/opentripsplit` org free, no exact web matches. Similar names exist ("TripSplit" iOS app, "Trip Split", "Trip Splitter"). Trademark not checked. Optionally claim the `opentripsplit` GitHub org to reserve the name.

**Status (2026-11-08):** build steps 1–8 implemented locally (not yet committed or pushed). Repo settings under "GitHub setup → Repo settings" still need to be applied by hand. Deviations from this plan, all small:
- `go.mod` keeps `go 1.27.2` and adds `toolchain go1.27.3` (1.27.2 has a crypto/x509 vulnerability, GO-2026-6809).
- Discord endpoints come from `golang.org/x/oauth2/endpoints` instead of being set by hand.
- Extra columns: `users.deleted_at`, `invites.id`. Extra env vars: `LISTEN_ADDR`, `INVITE_TTL`.
- Dependabot also watches the Dockerfile base images.
- No screenshot in the README yet.
- Added after the plan: login providers can be configured in the browser. With no `GITHUB_*`/`DISCORD_*` env vars, the server logs a setup token; `/setup` takes it plus OAuth credentials, and the next login from that browser becomes the instance admin (`users.is_admin`), who edits providers at `/admin`. Env vars take precedence and are read-only in the UI. UI-entered secrets live in a new `settings` table (migration `0002`).
- Added after the plan: `DEV_LOCAL_AUTH` dev login (refused unless `BASE_URL` is loopback) with three preset test users (Mike, Chris, Ryan), free-name login, a "Switch user" link, and an owner-only button that adds the test users to a group. The setup claim cookie is now scoped to `/` so a dev login can claim admin.
- Added after the plan: indie pixel-game theme tuned for keratoconus. Night/Parchment palettes (AAA text contrast, no pure black/white, enforced by `theme_test.go`), Pixelify Sans for headings/buttons, Atkinson Hyperlegible Next for body text (self-hosted, OFL), pixel-art SVG icons, and per-browser Theme/Text size settings via `POST /prefs` cookies.

## Decisions (confirmed)
- **UI:** Go server-rendered HTML (`html/template`) + htmx. One binary, no JS build.
- **Login:** GitHub and Discord via OAuth2. No passwords stored.
- **Splits:** equal, exact amounts, percent, and shares.
- **Members:** registered users only. Everyone in a split logs in with GitHub or Discord (no name-only placeholder members).
- **License:** MIT.

## Defaults I picked (change if you disagree)
- **Database:** SQLite via `modernc.org/sqlite` (pure Go, no C compiler needed on Windows). One file on disk. Can move to Postgres later.
  - DSN pragmas on every connection: `_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)`. Foreign keys are off by default in SQLite.
  - One writer connection (`SetMaxOpenConns(1)`) to avoid `SQLITE_BUSY`. Add a separate read pool only if needed.
- **Routing:** standard library `net/http` (method + path patterns). No web framework.
- **OAuth:** `golang.org/x/oauth2` with PKCE (`oauth2.GenerateVerifier`). GitHub has a built-in endpoint; Discord endpoints set by hand.
- **Money:** integer **minor units** as `int64` (cents for USD, yen for JPY, fils for KWD). A small built-in currency table maps code → decimal places; a group picks its currency from that list. One currency per group, no exchange rates.
- **Parsing amounts:** "12.50" → 1250 with string/integer code, never `strconv.ParseFloat`.
- **Timestamps:** UTC, stored as integer unix seconds.
- **Go version:** 1.27, latest patch (`go 1.27` in `go.mod`). CI reads the version from `go.mod` (`actions/setup-go` with `go-version-file: go.mod`), so local and CI stay in sync.

**v1 limits (by design):** one payer per expense, one currency per group, no placeholder members, no receipts/attachments.

## Privacy / data minimization
Provider user IDs and display names are still personal data (e.g. under GDPR), so the goal is "store as little as possible", not "no PII".
- GitHub: request **no scopes**; call `GET /user`, keep only numeric `id` and `login`.
- Discord: request only `identify` scope (no `email`); call `GET /users/@me`, keep `id` and `global_name` (fall back to `username` when null).
- Store: provider name, provider user ID, display name. No email, no avatar, no tokens kept after login.
- No IP addresses in logs. README gets a short privacy note saying what is stored.
- **Account linking:** Account page has "Link GitHub/Discord", which adds an `identities` row to the logged-in user, so one person signing in both ways doesn't end up with two accounts.
- **Delete my account:** remove identity rows and sessions, rename user to "Former member #N" (N unique per instance) so group balances still add up and several former members stay distinguishable. If they own a group, ownership passes to the longest-standing remaining member.
- `audit_log` JSON stores user IDs only, never names. Names are looked up when the log is rendered, so deletion leaves no old name behind.

## Data model (SQLite tables)
- `users` (id, display_name, created_at)
- `identities` (provider, provider_user_id, user_id) — unique on (provider, provider_user_id)
- `sessions` (token_hash, user_id, expires_at) — cookie holds a random 32-byte token; DB holds only its SHA-256. Expired rows purged at startup and hourly.
- `groups` (id, name, currency, created_by, created_at, archived_at)
- `group_members` (group_id, user_id, role [owner/member], joined_at, left_at)
- `invites` (token_hash, group_id, created_by, expires_at, revoked_at) — join by link
- `expenses` (id, group_id, payer_id, description, amount_minor, split_type, spent_on, created_by, created_at, updated_at, deleted_at)
- `expense_shares` (expense_id, user_id, input_value, amount_minor) — `input_value` is an INTEGER holding the raw entry: basis points for percent (sum = 10000), integer weights for shares, minor units for exact (sum = amount), unused for equal. `amount_minor` is the computed result.
- `settlements` (id, group_id, from_user, to_user, amount_minor, created_by, created_at, deleted_at)
- `audit_log` (id, group_id, actor_id, action, entity, entity_id, before_json, after_json, at)

Deletes are soft (`deleted_at`). Every create/edit/delete of expenses and settlements, and every member join/leave/remove, writes an `audit_log` row. That's how you resolve discrepancies in past groups.

## Permissions
- Every `/groups/{id}/...` route goes through one `requireMember` helper. A non-member gets **404** (not 403), so group IDs can't be probed.
- Any member can add expenses. The expense creator or the group owner can edit/delete it.
- Either party (payer or receiver) can record a settlement. The creator or the owner can delete it.
- Owner: rename group, create/revoke invite links, remove members, archive/unarchive, transfer ownership.
- A member can leave, or be removed, only when their balance is 0. Members with a nonzero balance stay until settled.
- Archived groups are read-only for everyone (only unarchive is allowed).

## Core logic (`internal/ledger`, pure functions, no DB)
1. **Split** an expense into per-person minor units for each split type. Leftover units from rounding go one at a time to members by largest remainder. Ties (always the case for equal splits) are broken by a member order rotated by expense ID, so the same person doesn't always get the extra cent. Totals always match exactly.
2. **Validate** inputs: percent basis points sum to 10000, exact amounts sum to the total, shares > 0, every participant is an active member.
3. **Balance** per member = paid − owed + settlements sent − settlements received. Sum across the group is always 0.
4. **Settle up**: greedy match of largest creditor with largest debtor until all balances are 0. At most (members − 1) payments.
5. "Settle up" screen shows suggested payments; a member taps "Mark paid", which records a `settlement`. Owner can **archive** a group: read-only, still viewable with full history.

## Project layout
```
cmd/opentripsplit/main.go      wiring, config from env, start server, `healthcheck` subcommand
internal/config/               env parsing (BASE_URL, DB_PATH, GITHUB_*, DISCORD_*, SESSION_*)
internal/auth/                 OAuth providers, login/callback, linking, sessions, middleware
internal/store/                DB open, queries
internal/store/migrations/     numbered .sql files, embedded (go:embed can't reach parent dirs) and run at startup
internal/ledger/               split, balances, settle-up, currency table (most tests live here)
internal/web/                  handlers, templates (embed.FS), static (vendored htmx.min.js)
```

## Security
- CSRF: Go's `http.CrossOriginProtection` (added in Go 1.25). It compares `Origin` with `Host`, so the README tells reverse-proxy users to pass `Host` through.
- Cookies: `HttpOnly`, `SameSite=Lax`, and `Secure` when `BASE_URL` is https (Safari drops Secure cookies on plain `http://localhost`). The OAuth `state` + PKCE verifier live in a short-lived Lax cookie; Strict would not be sent on the provider redirect.
- Session token rotated on every login; logout deletes the row.
- Headers: `Content-Security-Policy: default-src 'self'` (no inline scripts or styles), `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`. Set `htmx.config.allowEval = false`.
- Expired or revoked invite tokens show a clear error page.

## Pages
Login (GitHub / Discord buttons) → My groups (active + archived) → Group page (expenses list, add/edit expense form with split-type switch via htmx, balances, settle-up suggestions, history/audit tab, members, invite link) → Account (display name, linked providers, delete account).

## Build order
1. ~~Create GitHub repo~~ (done). `go mod init github.com/espm1000/opentripsplit`, add `.gitignore` entries `*.db`, `*.db-wal`, `*.db-shm`, `/opentripsplit`, CI green on an empty `main.go`.
2. `internal/ledger` + full table-driven tests (no DB, no web).
3. Store: migrations + queries.
4. Auth: GitHub, then Discord; sessions; logout; account linking.
5. Groups + invite links (create/revoke) + membership (leave/remove/transfer) + `requireMember`.
6. Expenses (all split types), balances, settle-up, settlements.
7. Audit log view, archive/unarchive, account deletion.
8. Dockerfile (distroless static, nonroot uid 65532, SQLite file on a mounted volume writable by that uid) + README deploy section.

## GitHub setup
**Create repo:** done — public `espm1000/opentripsplit` with Go `.gitignore` and MIT `LICENSE`.

**Files to add:**
- `README.md` — what it is, screenshot, quick start, how to create GitHub/Discord OAuth apps, env vars, privacy note, deploy (reverse proxy `Host`, backups)
- `CONTRIBUTING.md` — how to run, test, and submit PRs
- `CODE_OF_CONDUCT.md` — GitHub's Contributor Covenant template
- `SECURITY.md` — "report privately via GitHub Security Advisories"
- `.env.example` — every env var, no real values
- `.golangci.yml` — golangci-lint v2 format (`version: "2"`)
- `.github/workflows/ci.yml` — on push/PR: `go vet`, `go test -race ./...`, `golangci-lint`, `govulncheck`
- `.github/dependabot.yml` — weekly updates for `gomod` and `github-actions`
- `.github/ISSUE_TEMPLATE/bug_report.yml`, `feature_request.yml`
- `.github/PULL_REQUEST_TEMPLATE.md`

**Repo settings:**
- Ruleset on `main`: require PR, require CI check to pass, block force push and deletion. Add yourself (repo admin) to the bypass list so solo work isn't blocked.
- Security: enable Dependabot alerts + security updates, secret scanning + push protection, private vulnerability reporting, CodeQL default setup (all free on public repos)
- Merge: allow squash merge only (clean history)
- Never commit OAuth client secrets; they go in env vars (and GitHub Actions secrets if ever needed)

## Deploy notes (for README)
- Backups: `sqlite3 app.db ".backup backup.db"` or `VACUUM INTO 'backup.db'`. Don't copy the file while the server runs in WAL mode.
- Distroless has no shell or curl, so the container `HEALTHCHECK` runs `opentripsplit healthcheck`.
- Run behind a TLS reverse proxy; set `BASE_URL` to the public https URL.

## License notes (MIT)
- Anyone may use, copy, change, sell, or host your code. Only requirement: keep the copyright + license text. No warranty.
- Someone could host a closed-source modified version. If that bothers you, AGPL-3.0 is the alternative; you chose MIT.
- Planned dependencies (`golang.org/x/oauth2`, `modernc.org/sqlite`, htmx) are under permissive licenses compatible with MIT. Confirm before first release with `go-licenses report ./...`.
- No CLA needed. Contributions are under MIT by default per GitHub's terms ("inbound = outbound").

## Verification
- `go test -race ./...` — ledger tests check: each split type, rounding leftovers (and that the tiebreak rotates), shares sum to amount, input validation, currencies with 0/2/3 decimal places, amount parsing without floats, balances sum to 0, settle-up clears all balances with ≤ n−1 payments.
- Store tests against a temp SQLite file, including that foreign keys are enforced.
- Handler tests with `httptest` and a fake OAuth server (no real GitHub/Discord in CI):
  - a non-member gets 404 on every group route
  - a member can't leave with a nonzero balance, and can with zero
  - writes to an archived group are rejected
  - after account deletion, the audit log view shows "Former member #N" and no old name
  - revoked/expired invites are rejected
- Manual: run locally with real GitHub + Discord OAuth apps (callback `http://localhost:8080/auth/{provider}/callback`), log in as 2–3 accounts, link both providers on one account, create a group, add expenses of each split type, settle up, archive, confirm history and audit log show every change, restart the server and confirm data persists, take a backup with `.backup` and restore it.
