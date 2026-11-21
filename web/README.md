# banyan web

The banyan web console: a React SPA (Vite + TypeScript + react-router +
TanStack Query + highlight.js) served by `banyand web` — the web tier of
the three-layer deployment (web → gateway → storage, `internal/webapp`).
The browser never talks to the gateway directly and never sees a
credential: the web tier keeps sessions in its own database
(sqlite / mysql / postgres) and attaches the session's Authorization
header to every proxied call — pages see exactly what the same user's
git client sees.

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
| `/` | the repository list, GitHub-shaped: name (group prefix), description (truncated), latest update |
| `/groups` | every group with its member repositories and deletion |
| `/groups/:name` | one group: the member table (what labeled repositories inherit — viewer/developer/owner) and the group's repositories |
| `/merge_requests` | the workspace-wide request list (state filter; rows link into the repository's page) |
| `/new?kind=repo\|group` | the topbar "+" target: a blank repository (group required, name, description, optional parent nest), an import from URL (with the job list), or a new group |
| `/r/:repo` or `/r/:group/:project` | every repository page answers both shapes — the group form is the canonical address, the one-level form keeps old links alive; the project segment is the physical id |
| `/r/:repo` (legacy) | branch list (**your** filtered view, parsed from the same ref advertisement git gets) + recent history |
| `/r/:repo/tree/…` | code browser: lazy one-level directory walk (`GET /api/v1/tree`), file contents with line numbers + syntax highlighting + binary detection + 64 KiB preview cap, branch switcher, blame links |
| `/r/:repo/history/:ref` | branch history with cursor paging and a path filter |
| `/r/:repo/commit/:oid` | commit metadata + the unified diff against its first parent |
| `/r/:repo/blame/…` | blame view: single-line attribution per commit run |
| `/r/:repo/branches` | branch management through the same ref CAS a git push rides |
| `/r/:repo/subtrees` | subtree management: directory timelines, branch heads, per-path activity, admin access rules |
| `/r/:repo/settings` | repo admin: size, force-push switch, members (effective table with inherited rows marked, direct grants strictly above the inherited rank), group label, rename, repack |
| `/settings/profile` | the settings area's Profile section: the theme (auto / day / night) and the language (auto / 中文 / English) |
| `/settings/ssh-keys` | the settings area's SSH Keys section: paste one authorized_keys line, list (type / fingerprint / comment / added — the key material never echoes), remove your own. The keys authenticate git-over-SSH as their owner (`ssh://git@host/<repo>.git`) |
| `/settings/tokens` | the settings area's Access Tokens section: mint (plaintext shown exactly once), list your own, list and revoke anyone's with admin |

Groups are organizational containers only (docs/design/data-model.md §15):
they folder the list and nothing else — the repository's physical name and
every URL stay untouched. The theme picker lives in the settings area's
Profile section (the pre-paint snippet in `index.html` still applies the
stored choice before first paint); both three-state pickers label their
first choice "auto" with the explanation in the hint below.

The console is bilingual (Chinese / English). The default follows the
browser's language (`zh*` tags pick Chinese, everything else English); the
picker in the settings area overrides it per browser (`localStorage
"banyan.locale"`, with "auto" stored by removing the key — the theme's
pattern). The dictionary is English-source-as-key: the English text is its
own fallback and `zh.ts` is the only table, so a missing entry degrades to
English instead of breaking; a vitest pins both placeholder parity and
that every static call-site key carries a zh entry. Menus and headings
wear Title Case (Repositories, Merge Requests, Access Tokens), acronyms
keep their caps (SSH Keys); labels, buttons and table columns stay
sentence case.

The chrome: the avatar at the top bar's right end opens the account menu
(click or right-click — identity, Settings, Log Out). The sidebar
is contextual, one group at a time: the workspace pages carry the three
global destinations (Repositories, Groups, Merge Requests) with one-key
shortcuts; inside /r/&lt;repo&gt; it carries only that repository's
navigation — the breadcrumb atop the content (Repositories / group / repo)
is the way back out; under /settings it carries the settings area's
sections (Profile, SSH Keys, Access Tokens — the brand link leads back
out). The fold control sits at the sidebar's bottom edge;
`[` works everywhere (plain keys only — never while typing, never with
modifiers; R/G/M stay live on repository and settings pages too).

In the `gitlab` profile the SPA narrows to the repositories list and each
repo's subtree pages; login is the GitLab provider only.
