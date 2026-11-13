# banyan web

The banyan web console: a **React SPA** (Vite + TypeScript + react-router +
TanStack Query + highlight.js) served by **`banyand web`** — the web tier
of the three-layer standalone deployment (web → gateway → storage,
`internal/webapp`). The browser never talks to the gateway directly and
never sees a credential: the web tier keeps **sessions in its own
database** (sqlite / mysql / postgres) and attaches the session's
Authorization header to every proxied call, so the pages see exactly what
the same user's git client sees — branch visibility, directory access,
everything.

```bash
# frontend
npm install
npm run build          # tsc (type-check) + vite build → dist/
npm test               # vitest (parsers, blame/diff, blob rendering)
npm run dev            # vite on :5173, proxying /api to the web tier on :3000

# web tier (from the repo root)
go run ./cmd/banyand web -listen :3000 -gateway http://127.0.0.1:8080 \
  -db sqlite:./web/web.db -dist ./web/dist
```

## The web tier (`banyand web`)

- **Database** (`-db`): `sqlite:<path>` (default), `mysql:<dsn>`,
  `postgres:<dsn>` — every table prefixed `web_`, so the web tier may
  share the gateway's database (the demo compose points both at the same
  sqlite file; WAL + busy_timeout + immediate transactions are the
  multi-process contract, `meta.SQLiteDSN`). Sessions and import history
  survive restarts; still-running imports at boot are marked failed
  ("interrupted").
- **Login**: local form (the gateway's `POST /api/v1/auth/login` stays the
  single check) and OAuth providers — GitHub and any number of GitLab
  instances. Provider config merges from `BANYAN_OAUTH_PROVIDERS` (env
  JSON), `OAUTH_PROVIDERS_FILE` (default `/config/oauth-providers.json`,
  `{"providers":[…]}`), and the two legacy single-GitLab shapes
  (`BANYAN_GITLAB_*` env, `/config/gitlab-oauth.json`) — de-duplicated by
  provider id. Scopes: GitLab `read_api read_repository`, GitHub `repo`
  (private-repo import needs it).
- **Profiles** (`-profile`): `standalone` (default — the full console and
  the single control plane) or `gitlab` (the console narrows to banyan's
  subtree features; GitLab keeps project management and code browsing).
  `/api/mode` advertises `{profile, sso, providers, capabilities}` and the
  SPA renders to that list.
- **Proxy**: `/api/v1/*` and the git ref advertisement
  (`/r/<repo>.git/info/refs`) are forwarded with the session's
  credentials; the path forwarded is BASE_PATH-stripped.
- **Repo import**: `POST /api/imports` (session token or pasted PAT;
  local passwords are never sent to a source host) drives the gateway's
  synchronous import API and tracks the job in `web_imports`;
  `GET /api/imports` lists them.

## Environment (container spellings)

| Variable | Flag | Purpose |
|---|---|---|
| `PORT` | `-listen` | listen port (bare number) |
| `BANYAN_URL` | `-gateway` | the gateway (banyand serve) base URL |
| `BANYAN_WEB_DB` | `-db` | `sqlite:<path>` \| `mysql:<dsn>` \| `postgres:<dsn>` |
| `BANYAN_WEB_PROFILE` | `-profile` | `standalone` \| `gitlab` |
| `DIST_DIR` | `-dist` | the SPA build directory |
| `BASE_PATH` | `-base-path` | mount under a URL prefix (GitLab `/-/banyan`) — stripped at request time; baked into the SPA at build time |
| `BANYAN_OAUTH_PROVIDERS` | — | provider list as env JSON (see above) |
| `OAUTH_PROVIDERS_FILE` | — | provider list file, default `/config/oauth-providers.json` |
| `BANYAN_GITLAB_URL` / `BANYAN_GITLAB_CLIENT_ID` / `BANYAN_GITLAB_CLIENT_SECRET` / `BANYAN_GITLAB_EXTERNAL_URL` | — | legacy single-GitLab provider (the demo provisioning keeps working) |
| `OAUTH_CONFIG_FILE` | — | legacy single-GitLab provider file, default `/config/gitlab-oauth.json` |

## Pages

| Route | What it shows |
|---|---|
| `/login` | local form + one button per OAuth provider |
| `/` | repository list + creation |
| `/r/:repo` | branch list (**your** filtered view, parsed from the same ref advertisement git gets) + recent history |
| `/r/:repo/tree/…` | code browser: lazy one-level directory walk (`GET /api/v1/tree`), file contents with line numbers + syntax highlighting + binary detection + 64 KiB preview cap, branch switcher, blame links |
| `/r/:repo/history/:ref` | branch history with cursor paging and a path filter |
| `/r/:repo/commit/:oid` | commit metadata + the unified diff against its first parent |
| `/r/:repo/blame/…` | blame view: single-line attribution per commit run |
| `/r/:repo/branches` | branch management through the same ref CAS a git push rides |
| `/r/:repo/subtrees` | subtree management: directory timelines, branch heads, per-path activity, admin access rules |
| `/r/:repo/settings` | repo admin: size, force-push switch, rename, repack |
| `/settings/tokens` | personal access tokens: mint (plaintext once), list, revoke |
| `/settings/imports` | repo import: source URL, session token or PAT, target name; job list with live status |

In the `gitlab` profile the SPA narrows to the repositories list and each
repo's subtree pages; login is the GitLab provider only.
