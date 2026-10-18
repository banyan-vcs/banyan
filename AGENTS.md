# AGENTS.md — Banyan Working Rules

Banyan: a git-compatible version-control **server** built for large-scale
subtree-parallel iteration in monorepos (design doc `docs/`, current model:
v4 timeline-first, see `docs/01`).

## Design Goal (top constraint)

**Target git compatibility; the git source is the sole behavioral reference.**

1. Match git behavior, not go-git behavior. When semantics are unclear, read
   the git source (`upload-pack.c` / `receive-pack.c` / `http-backend.c` /
   `Documentation/technical/*.adoc`) — do not guess.
2. Settle disputes with captured reference traffic: a standard git
   `http-backend` behind a tiny CGI shim (kept in the test workspace, dumps
   both directions byte-level) plus `GIT_TRACE_PACKET` on the client side.
   Protocol-level cases replay via hand-crafted pkt-line requests
   (`rawPush` in `integration/`).
3. go-git is a library, not a boundary. Only `internal/storage/packfile.go`
   uses it (pack decode + no-delta encode). Where it falls short (protocol
   v2, server-side filter, deltas), implement against the git source instead.
   Acceptance for any replacement: byte-level behavioral parity with git.
4. Advertise only fully implemented capabilities (not advertising a
   capability makes git clients disable or downgrade it). Track accepted
   divergences in `docs/03` §8–§10; register new ones there.

## Conventions

- Build & test: `go build ./... && go test ./...` (integration tests need
  `git` on PATH). Run the server via `go run ./cmd/banyand`.
- Commits: layered (docs → model → storage → gitwire → test), each one
  buildable on its own; the message explains *why* and cites the git-source
  or reference-trace basis.
- Tests: every new capability gets an integration case, registered in the
  matrix in `docs/07`. Prefer blob-hash assertions over worktree diffs
  (line-ending noise). Protocol bugs surface only under the real git CLI.
- Comments: English first, stating constraints the code cannot show.
