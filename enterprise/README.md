# banyan enterprise

Commercial capabilities, maintained as an **independent Go module** (own
`go.mod`; the MIT core is consumed via a local `replace` — the dependency
is strictly one-way, core never imports enterprise). Licensed under
[LICENSE.enterprise](LICENSE.enterprise): free to modify and test,
production use requires a banyan enterprise subscription.

## Structure

- `license/` — the plan gate every commercial capability checks through:
  Ed25519-signed license tokens (stdlib only), plan→feature matrix, FREE
  default when no valid license is present.
- `cmd/banlicense` — issue licenses from a vendor private key; bootstraps
  the keypair on first run (keep the private half offline).
- `wiring/` — a compile-time probe pinning the module boundary: enterprise
  MAY import the core's packages including `internal/` (the internal
  visibility rule is import-path based and this module lives under the
  core's path tree). If that probe breaks, the boundary design broke.

Feature capabilities (audit, SSO, DB-backed policy, DR, governance,
quotas) land here incrementally; the catalog and rollout plan live with
the vendor's enterprise design doc, not in this repository.

## Build & test

```bash
cd enterprise
go mod tidy        # resolves the core via ../
go test ./...
go run ./cmd/banlicense -key vendor.pem -plan enterprise -days 30
```
