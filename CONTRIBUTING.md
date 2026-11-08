# Contributing

Thanks for helping! Bug reports, fixes and small features are all welcome. For anything big, please open an issue first so we can agree on the approach.

## Running it

1. Install Go 1.27 or newer. The `toolchain` line in `go.mod` downloads the exact version for you if needed.
2. Create a GitHub or Discord OAuth app with callback `http://localhost:8080/auth/<provider>/callback` (see the README).
3. Run `go run ./cmd/opentripsplit`, open http://localhost:8080/setup, and enter the setup token from the log plus your OAuth app's credentials. Alternatively, put them in `.env` (copy `.env.example`) and load it with `set -a; . ./.env; set +a` before starting.

**Quickest:** skip OAuth with dev login: `DEV_LOCAL_AUTH=1 LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/opentripsplit`. Log in as Alice, Bob or Casey (or any name), use **Switch user** to change people, and use the group's **Add Alice, Bob and Casey** button. See the README for details.

To try real logins with several people, use different browsers or private windows.

## Tests and checks

```sh
go test -race ./...
golangci-lint run
govulncheck ./...
```

CI runs all three on every pull request. The handler tests use a fake OAuth server, so no real accounts or network are needed.

Where tests live:

- `internal/ledger`: table-driven and randomized tests for the money logic. Any change to splitting, rounding or settle-up needs tests here.
- `internal/store`: tests against a temporary SQLite file, including permission rules.
- `internal/web`: end-to-end HTTP tests through the real login flow.

## Rules of the codebase

- **Money is integer minor units.** Never use `float64` for amounts. Parse input with `ledger.ParseAmount`.
- **Permissions are checked in the store**, inside the write's transaction. Handlers may check early for a nicer error, but the store is the source of truth.
- **Every group route goes through `requireMember`**: add new ones to `groupRoutes()` in `internal/web/server.go`. The non-member 404 test covers that list automatically.
- **Audit everything:** create/edit/delete of expenses and settlements and membership changes write an `audit_log` row. Audit payloads hold user IDs, never names.
- **Store as little personal data as possible.** No new provider fields, no IP addresses in logs.
- **No inline scripts or styles:** the Content-Security-Policy blocks them.
- **Schema changes** go in a new numbered file in `internal/store/migrations/`. Never edit a migration that has been released.

## Pull requests

- Keep each PR focused on one thing, and describe what changed and why.
- Make sure tests and lint pass.
- PRs are squash-merged, so the PR title becomes the commit message.

By contributing, you agree that your contributions are licensed under the MIT License, the same as the project.

Please follow the [Code of Conduct](CODE_OF_CONDUCT.md).
