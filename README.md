# OpenTripSplit

A small, self-hostable web app for splitting trip costs with friends. Everyone logs who paid for what. When the trip is over, the app tells the group who owes whom, using as few payments as possible.

- **Groups** with one currency each. Old trips are archived but stay fully viewable.
- **Four ways to split:** equally, exact amounts, percentages, or shares (e.g. 2 for someone who counts double).
- **Exact money:** amounts are stored as integer minor units (cents, yen, fils), never floats. Rounding leftovers are spread fairly and every split adds up exactly ([how](#how-splits-are-calculated)).
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

The login page then offers **Log in as Mike (dev) / Chris (dev) / Ryan (dev)**, plus a box to log in under any name (the same name always reaches the same account). A **Switch user** link in the header lets one browser hop between people. On a group's page, the owner gets a **Add Mike, Chris and Ryan** button that puts the three test users in the group (through a one-time invite, so it shows in the history like a normal join).

To test the admin pages too, complete `/setup` with the setup token (any dummy client ID and secret will do) and then dev-log in from the same browser; that user becomes admin.

Safety:

- It is **off by default**. The server **refuses to start** with `DEV_LOCAL_AUTH` on unless `BASE_URL` is `localhost`, `127.0.0.1` or `[::1]`. A production instance needs a public `BASE_URL` for OAuth callbacks, so the two can't be combined.
- While it's on, the log warns at startup and every page shows a red banner.
- Anyone who can reach the port can log in as anyone. Bind to loopback (`LISTEN_ADDR=127.0.0.1:8080`), or with Docker publish only on `127.0.0.1` (`-p 127.0.0.1:8080:8080`).
- Dev accounts use their own provider name (`dev`), so they never collide with real GitHub or Discord accounts.

## Trip simulator (demo and math check)

```sh
SIMULATOR=1 go run ./cmd/opentripsplit
```

This adds a public **Simulator** page at `/simulator`; no login is needed. Pixel travelers pass coins to each other while a retro terminal logs each step. It plays a random trip one step at a time:

- expenses with every split type
- the odd part payment mid-trip
- the suggested settle-up payments, until everyone is square

You can step through it or let it autoplay. Each step runs the real `internal/ledger` code and checks the math:

- splits add up and round fairly
- balances sum to zero
- settle-up clears every balance

A failed check is shown on the page and logged.

It is **off by default**. When on, it is stateless and **saves nothing**: each trip comes from a seed in the URL, so the same link always replays the same trip. That makes a link to any step good for sharing or for reproducing a bug. The same checks run on thousands of seeds in `go test ./internal/sim`, and `go test -fuzz=FuzzRun ./internal/sim` keeps looking for more.

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
| `SIMULATOR` | off | Public demo at `/simulator` that plays random trips through the split/balance math. Nothing is saved. |
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

## Look and accessibility

The UI has an indie pixel-game look, tuned for people with **keratoconus** (and low vision in general):

- **Two palettes:** *Night* (warm charcoal) and *Parchment* (cream). Neither uses pure black or pure white. Every text colour is at least 7:1 contrast (WCAG AAA), but peak contrast is kept around 13–14:1 rather than 21:1, which cuts halos and glare. Saturated neon pairs are avoided, and colour never carries meaning alone (balances also show ▲/▼ and "owes"/"is owed"). `internal/web/theme_test.go` fails the build if a colour change breaks these targets.
- **Fonts:** the pixel font (*Pixelify Sans*) is used only for large text: headings, buttons and the logo. Everything you read closely (amounts, descriptions, forms) uses *Atkinson Hyperlegible Next*, a typeface designed for low vision.
- **Settings in the footer of every page:** Theme (Match device / Night / Parchment) and Text size (Normal 18px / Large 20px / Huge 24px). They're saved per browser in cookies and applied on the server, so pages never flash the wrong theme, and they work without JavaScript. The whole layout scales with text size, and tables stack into rows on narrow screens.
- **High scores:** each group's balances are shown as an arcade high-score board: everyone ranked from most owed (1ST ★) to biggest debt, with signed amounts as the "score". Scores use the body font, because in the pixel font "5" and "S" look alike. Entries switch to two lines when the card is narrow, so names never get squeezed.
- **No visual noise:** no textures, scanlines, glow or blur. The only motion is a 1-frame button press, which `prefers-reduced-motion` turns off. Keyboard focus is a thick dashed outline.

Fonts are self-hosted (the Content-Security-Policy allows only same-origin resources) under the SIL Open Font License 1.1; see `internal/web/static/fonts/OFL-*.txt`. The pixel-art icons are original SVGs in `internal/web/static/icons/`.

## Privacy

OpenTripSplit stores as little as it can:

- From GitHub: your numeric user ID and login. From Discord: your user ID and display name.
- A display name you can change, plus the groups, expenses and payments you take part in.
- OAuth app client secrets, if the admin enters them in the UI (instance settings, not user data).
- **Not stored:** email, avatar, passwords, or OAuth access tokens. Tokens are dropped right after login.
- Display preferences (theme, text size) are stored only in your browser's cookies.
- Request logs record method, path, status and duration. They contain **no IP addresses**, and invite tokens are redacted.
- **Delete account** removes your logins and sessions. In shared groups you show as "Former member #N" so balances still add up. The audit log stores user IDs, never names, so your old name doesn't survive there either.

A user ID and display name still count as personal data under laws like the GDPR. If you run a public instance, you are responsible for meeting those obligations.

## How splits are calculated

Amounts are integers in the currency's smallest unit (cents, yen, fils), so $33.33 is 3333. Each expense is split like this:

1. Work out everyone's exact share: total × their weight ÷ all weights. The weight is 1 each for an equal split, the percentage for a percent split, or the number of shares for a shares split. Exact splits use the amounts as entered.
2. Round everyone down. That usually leaves a few units over, never more than one per person.
3. Give the leftover units, one each, to the people whose shares lost the most in rounding (the largest remainders).
4. If remainders tie, people take turns in user-ID order. Each expense starts the queue at a different person, chosen by its expense ID, so the same person doesn't always pay the extra cent.

For example, $100.00 split three ways is 3333.33… cents each. Everyone gets 3333, and the cent left over goes to whoever's turn it is: one expense gives it to the first person, the next to the second, and so on. The result always adds up to the total exactly, and nobody is ever more than one cent away from their exact share.

### How the math is verified

The split tests are in `internal/ledger/split_test.go`:

- **Hand-checked examples:** 17 splits across every split type and the zero- and three-decimal currencies (yen, fils), plus a whole trip followed from expenses to balances to settle-up. Every expected number is worked out in a comment, so you can check it with a calculator.
- **Independent checker:** a second implementation recomputes each exact share with arbitrary-precision arithmetic (`math/big`). It accepts only the one result the rules above allow. It runs on 20,000 random splits, from 1 cent up to the maximum amount, and at the int64 limits.
- **Taking turns:** tests confirm that the leftover moves one person along per expense, wraps from the last person back to the first, and evens out exactly over consecutive expenses. They also confirm it only ever breaks ties (it never overrides a larger remainder), handles any rotation value including negative and extreme ones, and ignores the order people are listed in.

To make sure the tests can actually fail, each common rounding bug was planted in turn (ignoring the rotation, reversing the tiebreak, smallest remainder first, an off-by-one in the turn order, and others). Every one made tests fail.

The tests log their working. A normal `go test` run stays quiet, and a failing test always prints its log. To see the working for passing tests, add `-v`:

```sh
go test -v -run 'Split|RoundRobin|Trip' ./internal/ledger/       # everything (~600 lines)
go test -v -run 'TestSplitWorkedExamples' ./internal/ledger/      # just the hand-checked splits
go test -v -run 'Split|RoundRobin|Trip' ./internal/ledger/ > split-verification.log
```

Each split prints a table of how its answer was reached:

```
shares split of 7 between 3 people, rotation 0
  user  weight  exact share  floor  remainder  turn  gets
     1       3        4.200      4        1/5     0     4
     2       1        1.400      1        2/5     1     2  +1
     3       1        1.400      1        2/5     2     1
floors sum to 6, leaving 1: largest remainders first, ties by turn (rotation 0 mod 3 = 0)
✓ sums to 7; everyone within one unit of their exact share; extras follow the rule
```

User 1 has the biggest share but the smallest remainder (0.2), so the leftover unit goes to users 2 and 3, who tie at 0.4. It's user 2's turn first.

Looping tests print summaries instead, including a grid showing who gets the extra units as the rotation changes (■ = extra unit):

```
3 left over, rotation 3: ■ · · ■ ■
```

Two behaviours to be aware of:

- **Turns are only fair across full ties.** In the 3 : 1 : 1 example above, user 1 never ties, so the turns are shared between users 2 and 3 only: user 2 gets the unit on 2 of every 3 rotations and user 3 on 1.
- **The rotation uses the global expense ID.** IDs are shared by every group on the server, so one group's expenses aren't always numbered consecutively. Over time the extra cents are fair on average, but not exactly even within each group.

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
internal/sim/              trip simulator: random trips through the ledger, with invariant checks
internal/web/              handlers, templates, static files (vendored htmx and simulator libraries)
```

The JavaScript and CSS libraries are vendored unmodified into `internal/web/static/`, because the CSP only allows same-origin files. They have no build step and Renovate doesn't track them, so update them by hand from the npm tarball:

- htmx 2.0.11 (`htmx.min.js`)
- Animate.css 4.1.1 (`animate.min.css`)
- `@formkit/auto-animate` 0.10.2 (`index.min.js`, saved as `auto-animate.min.js`)
- typewriter-effect 2.22.0 (`dist/core.js`, saved as `typewriter.min.js`). Stay on 2.x: it is MIT, and the `skipAddStyles` option keeps it CSP-clean.
- pixelarticons 2.4.2 (four SVGs in `sprites/`)

Their licenses are next to them.

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
