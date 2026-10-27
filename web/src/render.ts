// render.ts — server-rendered HTML for the demo web UI. One inline stylesheet,
// no client framework, no build artifacts beyond tsc. Every page shows WHO is
// logged in, because the whole demo is about per-user visibility.

import type { LogEntry, RepoInfo, RefEntry, TreeEntry } from "./banyan";

export function esc(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function layout(title: string, user: string, body: string): string {
  return `<!doctype html>
<html><head><meta charset="utf-8"><link rel="icon" href="/avatar.png" type="image/png"><title>${esc(title)} · banyan</title>
<style>
 body{font-family:ui-sans-serif,system-ui,sans-serif;margin:0;background:#f6f7f9;color:#1c2430}
 header{background:#16351f;color:#fff;padding:10px 20px;display:flex;align-items:center;gap:16px}
 header a{color:#cfe8d2;text-decoration:none}
 header .brand{font-weight:700;color:#fff;font-size:18px}
 main{max-width:960px;margin:24px auto;padding:0 16px}
 table{border-collapse:collapse;width:100%;background:#fff;border:1px solid #e2e5ea;border-radius:6px}
 th,td{text-align:left;padding:8px 12px;border-bottom:1px solid #eef0f3;font-size:14px}
 tr:last-child td{border-bottom:none}
 .muted{color:#68758a}
 .pill{display:inline-block;background:#e8f0ea;color:#16351f;border-radius:10px;padding:1px 8px;font-size:12px;margin-left:6px}
 pre{background:#fff;border:1px solid #e2e5ea;border-radius:6px;padding:12px;overflow:auto;font-size:13px}
 .btn{background:#16351f;color:#fff;border:none;border-radius:6px;padding:6px 14px;font-size:14px;cursor:pointer}
 input{border:1px solid #ccd2da;border-radius:6px;padding:6px 10px;font-size:14px;margin-right:6px}
 .crumbs a{color:#16351f}
 form.inline{display:inline}
</style></head><body>
<header><span class="brand"><img src="/avatar.png" alt="" style="width:22px;height:22px;border-radius:5px;vertical-align:-5px"> banyan</span>
 <a href="/">repositories</a>
 <span style="flex:1"></span>
 ${user ? `<span class="pill">${esc(user)}</span><form class="inline" method="post" action="/logout"><button class="btn" style="background:#2c5e3d">log out</button></form>` : `<a href="/login">log in</a>`}
</header><main>${body}</main></body></html>`;
}

export function loginPage(msg?: string): string {
  return layout(
    "log in",
    "",
    `<h2>Log in</h2>
${msg ? `<p style="color:#a33">${esc(msg)}</p>` : ""}
<form method="post" action="/login">
 <input name="user" placeholder="user (alice / bob / admin)" required>
 <input name="password" type="password" placeholder="password" required>
 <button class="btn">log in</button>
</form>
<p class="muted">The demo ships alice / bob (developers) and admin. Passwords live in banyan's auth config; this page checks them through banyan's login endpoint — the web tier stores no password table.</p>`
  );
}

export function repoListPage(user: string, repos: RepoInfo[], activeUser: string): string {
  const rows = repos
    .map(
      (r) => `<tr><td><a href="/r/${esc(r.id)}">${esc(r.id)}</a></td>
 <td class="muted">${esc(r.defaultBranch || "main")}</td></tr>`
    )
    .join("");
  return layout(
    "repositories",
    user,
    `<h2>Repositories</h2>
<table><tr><th>repository</th><th>default branch</th></tr>${rows || `<tr><td class="muted" colspan="2">none yet — create one below or push to auto-create</td></tr>`}</table>
<h3 style="margin-bottom:6px">Create repository</h3>
<form method="post" action="/r/new">
 <input name="name" placeholder="name" required pattern="[A-Za-z0-9._-]+">
 <button class="btn">create</button>
</form>
<p class="muted">Signed in as <b>${esc(activeUser)}</b>: the branch lists you see on each repository are YOUR view — personal branches of other users are invisible (docs/07 §3).</p>`
  );
}

export function repoPage(
  user: string,
  repo: string,
  refs: RefEntry[],
  defaultBranch: string,
  logEntries: LogEntry[]
): string {
  const branches = refs.filter((r) => r.name.startsWith("refs/heads/"));
  const others = refs.filter((r) => !r.name.startsWith("refs/heads/"));
  const branchRows = branches
    .map((b) => {
      const short = b.name.replace("refs/heads/", "");
      const personal = short.includes("/") ? `<span class="pill">personal</span>` : "";
      return `<tr><td><a href="/r/${esc(repo)}/tree?ref=${encodeURIComponent(short)}">${esc(short)}</a>${personal}</td>
 <td class="muted"><code>${esc(b.oid.slice(0, 10))}</code></td></tr>`;
    })
    .join("");
  const logRows = logEntries
    .map(
      (e) =>
        `<tr><td><code>${esc(e.oid.slice(0, 10))}</code></td><td>${esc(e.subject)}</td><td class="muted">${esc(e.author)}</td></tr>`
    )
    .join("");
  return layout(
    repo,
    user,
    `<h2>${esc(repo)}</h2>
<p class="muted">clone (with your credentials): <code>git clone http://<b>user:pass</b>@…/r/${esc(repo)}.git</code> · subtree: <code>/r/${esc(repo)}.git/tree/&lt;path&gt;</code></p>
<h3>Branches <span class="muted" style="font-weight:400">(your view — ${branches.length} visible)</span></h3>
<table><tr><th>branch</th><th>tip</th></tr>${branchRows || `<tr><td class="muted" colspan="2">empty</td></tr>`}</table>
${others.length ? `<p class="muted">also: ${others.map((o) => esc(o.name)).join(", ")}</p>` : ""}
<h3>Recent commits <span class="muted" style="font-weight:400">(${esc(defaultBranch || "main")})</span></h3>
<table><tr><th>commit</th><th>subject</th><th>author</th></tr>${logRows || `<tr><td class="muted" colspan="3">no history yet</td></tr>`}</table>`
  );
}

export function treePage(
  user: string,
  repo: string,
  ref: string,
  path: string,
  entries: TreeEntry[],
  parent: string | null
): string {
  const rows = entries
    .map((e) => {
      const child = path ? `${path}/${e.name}` : e.name;
      const href = `/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(child)}`;
      return `<tr><td>${e.isDir ? "&#128193;" : "&#128196;"} <a href="${href}">${esc(e.name)}</a></td><td class="muted">${esc(e.mode)}</td><td class="muted"><code>${esc(e.oid.slice(0, 10))}</code></td></tr>`;
    })
    .join("");
  const crumbs = [`<a href="/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}">${esc(ref)}:/</a>`]
    .concat(
      path
        .split("/")
        .filter(Boolean)
        .map((seg, i, arr) => {
          const p = arr.slice(0, i + 1).join("/");
          return `<a href="/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(p)}">${esc(seg)}/</a>`;
        })
    )
    .join(" ");
  return layout(
    `${repo}:${path}`,
    user,
    `<h2>${esc(repo)} <span class="muted" style="font-weight:400">@ ${esc(ref)}</span></h2>
<p class="crumbs">${crumbs}</p>
${parent != null ? `<p><a href="/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(parent)}">&larr; parent directory</a></p>` : ""}
<table><tr><th>name</th><th>mode</th><th>oid</th></tr>${rows}</table>`
  );
}

export function blobPage(user: string, repo: string, ref: string, path: string, content: string, truncated: boolean): string {
  const parent = path.split("/").slice(0, -1).join("/");
  return layout(
    `${repo}:${path}`,
    user,
    `<h2>${esc(repo)} <span class="muted" style="font-weight:400">@ ${esc(ref)}</span></h2>
<p class="crumbs"><a href="/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}">${esc(ref)}:/</a> <a href="/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(parent)}">${esc(path)}</a></p>
<pre>${esc(content)}${truncated ? "\n… (truncated for display)" : ""}</pre>`
  );
}

export function errorPage(user: string, status: number, text: string): string {
  return layout("error", user, `<h2>${status}</h2><p>${esc(text)}</p>`);
}
