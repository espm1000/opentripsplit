## What and why

<!-- What does this change, and what problem does it solve? Link the issue if there is one. -->

## How I tested it

<!-- Commands you ran, manual steps, screenshots for UI changes. -->

## Checklist

- [ ] `go test -race ./...` passes
- [ ] `golangci-lint run` passes
- [ ] Money stays in integer minor units (no floats)
- [ ] New group routes go through `requireMember` (`groupRoutes` in `internal/web/server.go`)
- [ ] No new personal data stored or logged
