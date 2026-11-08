# OpenTripSplit

A small, self-hostable web app for splitting trip costs with friends. Everyone logs who paid for what. When the trip is over, the app tells the group who owes whom, using as few payments as possible.

- **Groups** with one currency each. Old trips are archived but stay fully viewable.
- **Four ways to split:** equally, exact amounts, percentages, or shares (e.g. 2 for someone who counts double).
- **Exact money:** amounts are stored as integer minor units (cents, yen, fils), never floats. Rounding leftovers are spread fairly and every split adds up exactly.
- **Settle up:** suggested payments; tap "Mark paid" to record one.
- **Full history:** every add, edit and delete is kept in an audit log, so discrepancies can be traced.
- **Login with GitHub or Discord.** No passwords and no email stored.
- **One binary, one SQLite file.** Server-rendered HTML with a little htmx; no JavaScript build step.

## Quick start (local)

You need Go 1.27+. No configuration is required to start:

```sh
go run ./cmd/opentripsplit
```

1. The log prints a **setup token** (`setup_token=...`) and the setup page URL.
2. Open http://localhost:8080/setup, paste the token, and enter the client ID and secret of a GitHub and/or Discord OAuth app (see below).
3. Log in. That first login becomes the **instance admin**, who can change login providers later on the **Admin** page.

Then create a group and share an invite link with friends.

Prefer environment variables? Set `GITHUB_*` and/or `DISCORD_*` (see `.env.example`) and the providers are on from the start. Variables always win over settings saved in the UI, and the UI shows them read-only.

## Local testing without OAuth apps (dev login)

For development you can skip GitHub/Discord entirely:

```sh
DEV_LOCAL_AUTH=1 LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/opentripsplit
```

The login page then offers **Log in as Alice (dev) / Bob (dev) / Casey (dev)**, plus a box to log in under any name (the same name always reaches the same account). A **Switch user** link in the header lets one browser hop between people. On a group's page, the owner gets a **Add Alice, Bob and Casey** button that puts the three test users in the group (through a one-time invite, so it shows in the history like a normal join).

To test the admin pages too, complete `/setup` with the setup token (any dummy client ID and secret will do) and then dev-log in from the same browser; that user becomes admin.

Safety:

- It is **off by default**. The server **refuses to start** with `DEV_LOCAL_AUTH` on unless `BASE_URL` is `localhost`, `127.0.0.1` or `[::1]`. A production instance needs a public `BASE_URL` for OAuth callbacks, so the two can't be combined.
- While it's on, the log warns at startup and every page shows a red banner.
- Anyone who can reach the port can log in as anyone. Bind to loopback (`LISTEN_ADDR=127.0.0.1:8080`), or with Docker publish only on `127.0.0.1` (`-p 127.0.0.1:8080:8080`).
- Dev accounts use their own provider name (`dev`), so they never collide with real GitHub or Discord accounts.

## Creating the OAuth apps

The callback URL is `$BASE_URL/auth/<provider>/callback`. Locally that's `http://localhost:8080/auth/github/callback` or `http://localhost:8080/auth/discord/callback`.

**GitHub:** go to Settings → Developer settings → OAuth Apps → **New OAuth App**. Set the homepage URL to your `BASE_URL` and the authorization callback URL as above. Copy the client ID and generate a client secret. No scopes are requested; the app only reads your public profile.

**Discord:** go to the [Developer Portal](https://discord.com/developers/applications) → **New Application** → OAuth2. Add the callback URL under Redirects and copy the client ID and client secret. Only the `identify` scope is requested.

Use separate OAuth apps for local development and production, since each app has its own callback URL.

## Configuration

All settings come from environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `BASE_URL` | `http://localhost:8080` | Public URL of the app. With `https`, cookies get the `Secure` flag. |
| `LISTEN_ADDR` | `:8080` | Address to listen on. |
| `DB_PATH` | `opentripsplit.db` | SQLite database file. |
| `SESSION_TTL` | `720h` | How long a login lasts. |
| `INVITE_TTL` | `168h` | How long invite links stay valid. |
| `DEV_LOCAL_AUTH` | off | Password-less dev login with test users. Refused unless `BASE_URL` is localhost. Never in production. |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | | GitHub OAuth app (optional; can be set in the UI). |
| `DISCORD_CLIENT_ID`, `DISCORD_CLIENT_SECRET` | | Discord OAuth app (optional; can be set in the UI). |

Login providers can come from environment variables, from the setup/admin UI, or both. Environment variables take precedence. Credentials entered in the UI are stored **in the database**, so protect the database file and its backups; use environment variables if you'd rather keep secrets out of it. Never commit secrets to the repository.

### Setup and the admin

While the instance has no admin, the server logs a random setup token at every start. Whoever enters it at `/setup` gets a short-lived cookie, and their next login becomes the admin. After that, `/setup` is closed and the admin manages login providers at `/admin`. If the admin deletes their account, a new setup token is logged and the process repeats.

## Deploying

### Docker

```sh
docker build -t opentripsplit .
docker volume create opentripsplit-data
docker run -d --name opentripsplit -p 127.0.0.1:8080:8080 \
  -v opentripsplit-data:/data \
  -e BASE_URL=https://split.example.com \
  opentripsplit
docker logs opentripsplit 2>&1 | grep setup_token   # then open /setup
```

Add `-e GITHUB_CLIENT_ID=... -e GITHUB_CLIENT_SECRET=...` (or the `DISCORD_*` pair) to configure providers by environment instead.

The image is distroless and runs as uid **65532**, with the database at `/data/opentripsplit.db`. A named volume works out of the box. If you bind-mount a host directory instead, make it writable by that uid (`sudo chown 65532:65532 /srv/opentripsplit`). The image has no shell or curl, so its `HEALTHCHECK` runs `opentripsplit healthcheck`.

### Behind a reverse proxy

Run it behind a TLS-terminating proxy and set `BASE_URL` to the public `https://` URL. The proxy must **pass the original `Host` header through**: cross-site request protection compares the browser's `Origin` with `Host`. Caddy and the usual nginx `proxy_set_header Host $host;` do this.

```
split.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

### Backups

The database runs in WAL mode, so **don't copy the file while the server is running**. Use SQLite's online backup instead:

```sh
sqlite3 /path/to/opentripsplit.db ".backup /backups/opentripsplit-$(date +%F).db"
# or: sqlite3 /path/to/opentripsplit.db "VACUUM INTO '/backups/opentripsplit.db'"
```

To restore, stop the server, replace the database file with the backup (and delete any `-wal`/`-shm` files next to it), then start the server again.

## Privacy

OpenTripSplit stores as little as it can:

- From GitHub: your numeric user ID and login. From Discord: your user ID and display name.
- A display name you can change, plus the groups, expenses and payments you take part in.
- OAuth app client secrets, if the admin enters them in the UI (instance settings, not user data).
- **Not stored:** email, avatar, passwords, or OAuth access tokens. Tokens are dropped right after login.
- Request logs record method, path, status and duration. They contain **no IP addresses**, and invite tokens are redacted.
- **Delete account** removes your logins and sessions. In shared groups you show as "Former member #N" so balances still add up. The audit log stores user IDs, never names, so your old name doesn't survive there either.

A user ID and display name still count as personal data under laws like the GDPR. If you run a public instance, you are responsible for meeting those obligations.

## Development

```sh
go test -race ./...     # unit, store and handler tests (fake OAuth server, no network)
golangci-lint run       # v2 config in .golangci.yml
govulncheck ./...
```

Layout:

```
cmd/opentripsplit/         main: config, wiring, server, healthcheck subcommand
internal/config/           environment parsing
internal/auth/             OAuth providers (PKCE), sessions, login/link/logout
internal/store/            SQLite access; migrations in internal/store/migrations/
internal/ledger/           pure money logic: currencies, splitting, balances, settle-up
internal/web/              handlers, templates, static files (vendored htmx)
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
