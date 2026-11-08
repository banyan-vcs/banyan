# banyan-web

Lightweight repository management, file browsing and subtree management for
banyan (docs/07 §5, docs/07 §8c, docs/20). TypeScript, zero runtime
dependencies — Node's `http`/`fetch`/`crypto`/`fs` only; `typescript` +
`@types/node` are dev-only.

```bash
npm install            # dev toolchain only
npm run build          # tsc → dist/
BANYAN_URL=http://127.0.0.1:8080 PORT=3000 npm start
```

Pages: login → repository list (+ creation) → repository page (the branch
list is the logged-in user's view — personal branches of other users are
invisible, enforced server-side) → path-crumbed file browser with blob
previews → **subtree management** (`/r/<repo>/subtrees`): every directory
timeline with its branch heads and last commits, per-subtree activity,
clone recipes (the `/tree/` face and, in GitLab mode, the
`refs/banyan/tree/<dir>` projection ref), and — for admins — the runtime
access-rule form (`/api/v1/repos/<repo>/subtree-access`, docs/07 §8c).

The authorization story is the design point: every banyan call carries the
session user's Basic credentials, so the pages see exactly what the same
user's git client sees. Credentials live only in server-side sessions.

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `3000` | listen port |
| `BANYAN_URL` | `http://127.0.0.1:8080` | the banyan server |
| `BASE_PATH` | `""` | mount everything under a URL prefix (no trailing slash), e.g. `/-/banyan` behind GitLab nginx — full-path passthrough, links/redirects/cookie path all follow |
| `BANYAN_GITLAB_URL` | — | GitLab base URL this process dials (token exchange, `/api/v4`); with the two client vars below, switches login to GitLab OAuth2 SSO (scope `read_api`) |
| `BANYAN_GITLAB_EXTERNAL_URL` | = `BANYAN_GITLAB_URL` | the GitLab URL the BROWSER reaches (authorize redirect) — differs on docker networks (web dials `http://gitlab:8180`, users see the public URL) |
| `BANYAN_GITLAB_CLIENT_ID` / `BANYAN_GITLAB_CLIENT_SECRET` | — | the OAuth2 application (or use the config file below instead of env) |
| `OAUTH_CONFIG_FILE` | `/config/gitlab-oauth.json` | demo provisioning writes `{url, external_url, client_id, client_secret}` here (dynamically minted application); env wins when both exist |

## Auth modes

**Local** (default): the login form checks banyan's `POST /api/v1/auth/login`
and the session holds the user's Basic credentials (demo: alice / bob /
admin, password `demo`).

**GitLab SSO** (configured as above): `/login` redirects to GitLab's
`/oauth/authorize` (state anti-CSRF), the callback exchanges the code and
identifies the user via `/api/v4/user`; the session's credential is the
user's GitLab token, validated by banyan's `GitLabSSO` policy
(`auth.gitlab_sso` in banyan's config) — the same credential shape git
uses. GitLab instance admins get the rule-management forms. In SSO mode
the UI also wears **GitLab's palette** (purple-700 actions, the dark top
bar — lifted from GitLab's own rendered CSS); standalone keeps banyan's
green.

In SSO mode the repository list additionally shows GitLab project names
(`sha256(project_id)` → `path_with_namespace`, fetched with the user's own
token — display only, naturally scoped to their visibility).
