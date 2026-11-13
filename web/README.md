# banyan-web

The banyan web console: a **React SPA** (Vite + TypeScript + react-router +
TanStack Query + highlight.js) served by a **zero-runtime-dependency Node
BFF** that owns sessions and proxies `/api/v1` to banyan. The browser never
talks to banyan directly and never sees credentials — the BFF attaches the
session user's Basic header to every upstream call, so the pages see exactly
what the same user's git client sees (branch visibility, directory access,
everything; docs/07 §5).

```bash
npm install            # toolchain + SPA dependencies (build-time only)
npm run build          # tsc (SPA type-check) + tsc (BFF) + vite build → dist/ + server-dist/
npm test               # vitest (parsers, blame/diff, blob rendering)
BANYAN_URL=http://127.0.0.1:8080 PORT=3000 npm run bff

# development (two terminals):
npm run bff:watch      # the BFF on :3000, rebuilding on change
npm run dev            # vite on :5173, proxying /api and /auth to :3000
```

## Pages

| Route | What it shows |
|---|---|
| `/login` | local form or GitLab SSO entry |
| `/` | repository list + creation |
| `/r/:repo` | branch list (**your** filtered view, parsed from the same ref advertisement git gets) + recent history |
| `/r/:repo/tree/…` | code browser: lazy one-level directory walk (`GET /api/v1/tree`), file contents with line numbers + syntax highlighting + binary detection + 64 KiB preview cap, branch switcher, blame links |
| `/r/:repo/history/:ref` | branch history with cursor paging and a path filter (only commits touching a subtree) |
| `/r/:repo/commit/:oid` | commit metadata + the unified diff against its first parent (add/del rows, per-file stats) |
| `/r/:repo/blame/…` | blame view: author/date/summary per line, grouped by commit (`--line-porcelain`) |
| `/r/:repo/branches` | branch management: create/delete through the same ref CAS a git push rides |
| `/r/:repo/subtrees` | subtree management: every directory timeline, branch heads, per-path activity, admin access rules (`/api/v1/repos/<r>/subtree-access`, docs/07 §8c) |
| `/r/:repo/settings` | repo admin: size, force-push switch, rename, repack (403 ⇒ read-only) |
| `/settings/tokens` | personal access tokens: mint (plaintext once), list, revoke; admins see everyone's |

## The BFF (server/)

One file, Node builtins only (`http`/`fs`/`crypto`):

- sessions in memory (HttpOnly cookie; credentials never leave the server
  except as the Basic header to banyan),
- `POST /api/login` / `POST /api/logout` / `GET /api/session` / `GET /api/mode`,
- `GET /api/auth/gitlab` + `/auth/callback` — the GitLab OAuth2 flow,
- `GET /api/gitlab-names` — `sha256(project_id)` → project path (display only),
- a reverse proxy for `/api/v1/*` and the git ref advertisement
  (`/r/<repo>.git/info/refs`),
- static file serving from `dist/` with an SPA fallback (every non-API GET is
  a route).

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `3000` | BFF listen port |
| `BANYAN_URL` | `http://127.0.0.1:8080` | the banyan server |
| `BASE_PATH` | `""` | mount everything under a URL prefix (no trailing slash), e.g. `/-/banyan` behind GitLab nginx — baked into the SPA at **build time** (`vite` base) and stripped by the BFF at request time |
| `BANYAN_GITLAB_URL` | — | GitLab base URL this process dials (token exchange, `/api/v4`); with the two client vars below, switches login to GitLab OAuth2 SSO (scope `read_api`) |
| `BANYAN_GITLAB_EXTERNAL_URL` | = `BANYAN_GITLAB_URL` | the GitLab URL the BROWSER reaches (authorize redirect) — differs on docker networks (web dials `http://gitlab:8180`, users see the public URL) |
| `BANYAN_GITLAB_CLIENT_ID` / `BANYAN_GITLAB_CLIENT_SECRET` | — | the OAuth2 application (or use the config file below instead of env) |
| `OAUTH_CONFIG_FILE` | `/config/gitlab-oauth.json` | demo provisioning writes `{url, external_url, client_id, client_secret}` here (dynamically minted application); env wins when both exist |

## Auth modes

**Local** (default): the login form checks banyan's `POST /api/v1/auth/login`
and the session holds the user's Basic credentials (demo: alice / bob /
admin, password `demo`).

**GitLab SSO** (configured as above): the login page offers *continue with
GitLab* → `/oauth/authorize` (state anti-CSRF) → the callback exchanges the
code and identifies the user via `/api/v4/user`; the session's credential is
the user's GitLab token, validated by banyan's `GitLabSSO` policy
(`auth.gitlab_sso` in banyan's config) — the same credential shape git uses.
GitLab instance admins get the rule-management forms. In SSO mode the UI
wears **GitLab's palette** (CSS variables swapped on `<body>`); standalone
keeps banyan's green. The repository list additionally shows GitLab project
names (`sha256(project_id)` → `path_with_namespace`, fetched with the user's
own token — display only, naturally scoped to their visibility).
