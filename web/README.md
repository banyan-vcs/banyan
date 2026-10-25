# banyan-web

Lightweight repository management and file browsing for the banyan demo
(docs/13 §5). TypeScript, zero runtime dependencies — Node's
`http`/`fetch`/`crypto` only; `typescript` + `@types/node` are dev-only.

```bash
npm install            # dev toolchain only
npm run build          # tsc → dist/
BANYAN_URL=http://127.0.0.1:8080 PORT=3000 npm start
```

Pages: login → repository list (+ creation) → repository page (the branch
list is the logged-in user's view — personal branches of other users are
invisible, enforced server-side) → path-crumbed file browser with blob
previews.

The authorization story is the design point: every banyan call carries the
session user's Basic credentials, so the pages see exactly what the same
user's git client sees. Credentials live only in server-side sessions; the
password table exists only in banyan's auth config and is checked through
`POST /api/v1/auth/login`.

In the Docker demo: `docker compose up -d` → http://localhost:8101
(users alice / bob / admin, password `demo`).
