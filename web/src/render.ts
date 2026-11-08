// render.ts — server-rendered HTML for the web UI. One inline stylesheet,
// no client framework, no build artifacts beyond tsc. Every page shows WHO is
// logged in, because the whole demo is about per-user visibility.
//
// All internal links go through u(): the service may sit under a URL base
// path (BASE_PATH, e.g. GitLab's nginx routing /-/banyan/ here) — set once
// at startup via setBase().

import type { LogEntry, RepoInfo, RefEntry, TreeEntry, SubtreeListing, SubtreeSummary, SubtreeRule } from "./banyan";

let BASE = "";

// setBase installs the URL base path ("" = root; "/-/banyan" = subpage).
export function setBase(b: string): void {
  BASE = b.replace(/\/+$/, "");
}

// The theme follows the deployment mode (setTheme at startup): GitLab mode
// wears GitLab's palette so the subpage reads as part of the platform —
// values lifted from GitLab's own rendered CSS (purple-700 #5b46b8 actions,
// purple-200 #c9beef header links, the dark top bar, the red warn family).
// Everything else keeps banyan's own green.
const THEMES: Record<string, Record<string, string>> = {
  banyan: {
    "--hd": "#16351f", "--hd-link": "#cfe8d2",
    "--accent": "#16351f", "--accent-hi": "#2c5e3d",
    "--pill-bg": "#e8f0ea", "--pill-fg": "#16351f",
    "--warn-bg": "#f4e8e8", "--warn-fg": "#7a2e2e",
  },
  gitlab: {
    "--hd": "#222733", "--hd-link": "#c9beef",
    "--accent": "#5b46b8", "--accent-hi": "#483698",
    "--pill-bg": "#f4f0fc", "--pill-fg": "#483698",
    "--warn-bg": "#feeef0", "--warn-fg": "#ac203a",
  },
};
let THEME = THEMES.banyan;

// setTheme picks the palette ("banyan" | "gitlab").
export function setTheme(name: string): void {
  THEME = THEMES[name] || THEMES.banyan;
}

// u prefixes an app-absolute path with the base.
export function u(p: string): string {
  return BASE + p;
}

export function esc(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function whenStr(unix?: number): string {
  if (!unix) return "";
  return new Date(unix * 1000).toISOString().slice(0, 16).replace("T", " ") + " UTC";
}

function layout(title: string, user: string, body: string): string {
  const vars = Object.entries(THEME).map(([k, v]) => `${k}:${v}`).join(";");
  return `<!doctype html>
<html><head><meta charset="utf-8"><link rel="icon" href="${u("/avatar.png")}" type="image/png"><title>${esc(title)} · banyan</title>
<style>
 :root{${vars}}
 body{font-family:ui-sans-serif,system-ui,sans-serif;margin:0;background:#f6f7f9;color:#1c2430}
 header{background:var(--hd);color:#fff;padding:10px 20px;display:flex;align-items:center;gap:16px}
 header a{color:var(--hd-link);text-decoration:none}
 header .brand{font-weight:700;color:#fff;font-size:18px}
 main{max-width:960px;margin:24px auto;padding:0 16px}
 table{border-collapse:collapse;width:100%;background:#fff;border:1px solid #e2e5ea;border-radius:6px}
 th,td{text-align:left;padding:8px 12px;border-bottom:1px solid #eef0f3;font-size:14px}
 tr:last-child td{border-bottom:none}
 .muted{color:#68758a}
 .pill{display:inline-block;background:var(--pill-bg);color:var(--pill-fg);border-radius:10px;padding:1px 8px;font-size:12px;margin-left:6px}
 .warn{display:inline-block;background:var(--warn-bg);color:var(--warn-fg);border-radius:10px;padding:1px 8px;font-size:12px;margin-left:6px}
 pre{background:#fff;border:1px solid #e2e5ea;border-radius:6px;padding:12px;overflow:auto;font-size:13px}
 .btn{background:var(--accent);color:#fff;border:none;border-radius:6px;padding:6px 14px;font-size:14px;cursor:pointer}
 .btn.danger{background:var(--warn-fg)}
 .btn.soft{background:var(--accent-hi)}
 .btn:hover{background:var(--accent-hi)}
 input{border:1px solid #ccd2da;border-radius:6px;padding:6px 10px;font-size:14px;margin-right:6px}
 .crumbs a{color:var(--accent)}
 form.inline{display:inline}
</style></head><body>
<header><span class="brand"><img src="${u("/avatar.png")}" alt="" style="width:22px;height:22px;border-radius:5px;vertical-align:-5px"> banyan</span>
 <a href="${u("/")}">repositories</a>
 <span style="flex:1"></span>
 ${user ? `<span class="pill">${esc(user)}</span><form class="inline" method="post" action="${u("/logout")}"><button class="btn soft">log out</button></form>` : `<a href="${u("/login")}">log in</a>`}
</header><main>${body}</main></body></html>`;
}

export function loginPage(msg?: string): string {
  return layout(
    "log in",
    "",
    `<h2>Log in</h2>
${msg ? `<p style="color:#a33">${esc(msg)}</p>` : ""}
<form method="post" action="${u("/login")}">
 <input name="user" placeholder="user (alice / bob / admin)" required>
 <input name="password" type="password" placeholder="password" required>
 <button class="btn">log in</button>
</form>
<p class="muted">The demo ships alice / bob (developers) and admin. Passwords live in banyan's auth config; this page checks them through banyan's login endpoint — the web tier stores no password table.</p>`
  );
}

export function repoListPage(user: string, repos: RepoInfo[], activeUser: string, display?: (id: string) => string): string {
  const rows = repos
    .map((r) => {
      const name = display && display(r.id) ? `<a href="${u(`/r/${esc(r.id)}`)}">${esc(display(r.id))}</a> <span class="muted">${esc(r.id)}</span>` : `<a href="${u(`/r/${esc(r.id)}`)}">${esc(r.id)}</a>`;
      return `<tr><td>${name}</td>
 <td class="muted">${esc(r.defaultBranch || "main")}</td></tr>`;
    })
    .join("");
  return layout(
    "repositories",
    user,
    `<h2>Repositories</h2>
<table><tr><th>repository</th><th>default branch</th></tr>${rows || `<tr><td class="muted" colspan="2">none yet — create one below or push to auto-create</td></tr>`}</table>
<h3 style="margin-bottom:6px">Create repository</h3>
<form method="post" action="${u("/r/new")}">
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
  logEntries: LogEntry[],
  displayName?: string
): string {
  const branches = refs.filter((r) => r.name.startsWith("refs/heads/"));
  const others = refs.filter((r) => !r.name.startsWith("refs/heads/"));
  const branchRows = branches
    .map((b) => {
      const short = b.name.replace("refs/heads/", "");
      const personal = short.includes("/") ? `<span class="pill">personal</span>` : "";
      return `<tr><td><a href="${u(`/r/${esc(repo)}/tree?ref=${encodeURIComponent(short)}`)}">${esc(short)}</a>${personal}</td>
 <td class="muted"><code>${esc(b.oid.slice(0, 10))}</code></td></tr>`;
    })
    .join("");
  const logRows = logEntries
    .map((e) => `<tr><td><code>${esc(e.oid.slice(0, 10))}</code></td><td>${esc(e.subject)}</td><td class="muted">${esc(e.author)}</td></tr>`)
    .join("");
  return layout(
    repo,
    user,
    `<h2>${displayName ? esc(displayName) : esc(repo)}${displayName ? ` <span class="muted" style="font-weight:400">${esc(repo)}</span>` : ""}</h2>
<p class="muted">clone (with your credentials): <code>git clone http://<b>user:pass</b>@…/r/${esc(repo)}.git</code> · <a href="${u(`/r/${esc(repo)}/subtrees`)}">subtrees</a> · subtree: <code>/r/${esc(repo)}.git/tree/&lt;path&gt;</code></p>
<h3>Branches <span class="muted" style="font-weight:400">(your view — ${branches.length} visible)</span></h3>
<table><tr><th>branch</th><th>tip</th></tr>${branchRows || `<tr><td class="muted" colspan="2">empty</td></tr>`}</table>
${others.length ? `<p class="muted">also: ${others.map((o) => esc(o.name)).join(", ")}</p>` : ""}
<h3>Recent commits <span class="muted" style="font-weight:400">(${esc(defaultBranch || "main")})</span></h3>
<table><tr><th>commit</th><th>subject</th><th>author</th></tr>${logRows || `<tr><td class="muted" colspan="3">no history yet</td></tr>`}</table>`
  );
}

// subtreesPage is the subtree management listing (docs/07 §8c, docs/20):
// every directory timeline of the repo, its branch heads with last commits,
// the projection-ref mark, and the access-rule state (admins only — rules
// === null means the viewer does not manage this repo).
export function subtreesPage(
  user: string,
  repo: string,
  listing: SubtreeListing,
  rules: SubtreeRule[] | null,
  displayName?: string
): string {
  const byPath = new Map((rules || []).map((r) => [r.path, r]));
  const rows = listing.subtrees
    .map((s) => {
      const rule = byPath.get(s.path);
      const mark = `${s.projected ? `<span class="pill">projected</span>` : ""}${rule ? `<span class="warn">restricted</span>` : ""}`;
      const branches = s.branches
        .map((b) => `<div><b>${esc(b.name)}</b> <span class="muted"><code>${esc(b.head.slice(0, 10))}</code> ${esc(whenStr(b.when))} · ${esc(b.subject || "")}</span></div>`)
        .join("");
      return `<tr><td><a href="${u(`/r/${esc(repo)}/subtree?path=${encodeURIComponent(s.path)}`)}"><b>${esc(s.path)}</b></a>${mark}</td>
 <td>${branches || `<span class="muted">—</span>`}</td></tr>`;
    })
    .join("");
  return layout(
    `${repo} subtrees`,
    user,
    `<h2>${displayName ? esc(displayName) : esc(repo)} <span class="muted" style="font-weight:400">· subtrees</span></h2>
<p class="muted">Every directory timeline of this repository — each is a repository of its own (<code>/r/${esc(repo)}.git/tree&lt;path&gt;</code>); <b>projected</b> ones are also served as <code>refs/banyan/tree/&lt;dir&gt;</code> projection refs.</p>
<table><tr><th>subtree</th><th>branches (head · last commit)</th></tr>${rows || `<tr><td class="muted" colspan="2">no subtree timelines yet — push into a subdirectory (or a /tree/&lt;path&gt; URL) to create one</td></tr>`}</table>
<p class="muted"><a href="${u(`/r/${esc(repo)}`)}">&larr; repository</a>${rules === null ? "" : " · click a subtree to browse its activity, clone recipes and access rules"}</p>`
  );
}

// subtreeDetailPage: one subtree — activity feed (path-filtered log), the
// two clone recipes, and the admin's access-rule form.
export function subtreeDetailPage(
  user: string,
  repo: string,
  sub: SubtreeSummary,
  defaultBranch: string,
  activity: LogEntry[],
  gitlabRecipe: string | null,
  rule: SubtreeRule | null,
  isAdmin: boolean,
  displayName?: string
): string {
  const actRows = activity
    .map((e) => `<tr><td><code>${esc(e.oid.slice(0, 10))}</code></td><td>${esc(e.subject)}</td><td class="muted">${esc(e.author)}</td></tr>`)
    .join("");
  const segs = sub.path.replace(/^\//, "");
  const top = segs.split("/")[0];
  const rules = rule
    ? `<h3>Access rule</h3>
<p class="muted">fetch AND push of ${esc(sub.path)} (and everything deeper) restricted to the listed users. Managed by instance admins (docs/07 §8c).</p>
<form method="post" action="${u(`/r/${esc(repo)}/subtree-access`)}">
 <input type="hidden" name="path" value="${esc(sub.path)}">
 <input name="users" value="${esc(rule.users.join(", "))}" style="width:360px" placeholder="allowed users (comma or space separated)">
 <button class="btn">save rule</button>
</form>
<form method="post" action="${u(`/r/${esc(repo)}/subtree-access/delete`)}">
 <input type="hidden" name="path" value="${esc(sub.path)}">
 <button class="btn danger">remove rule (open to all)</button>
</form>`
    : isAdmin
      ? `<h3>Access rule</h3>
<p class="muted">Open — any authenticated user may fetch and push here.</p>
<form method="post" action="${u(`/r/${esc(repo)}/subtree-access`)}">
 <input type="hidden" name="path" value="${esc(sub.path)}">
 <input name="users" placeholder="allowed users (comma or space separated)" style="width:360px" required>
 <button class="btn">restrict to users</button>
</form>`
      : "";
  return layout(
    `${repo}${sub.path}`,
    user,
    `<h2>${displayName ? esc(displayName) : esc(repo)} <span class="muted" style="font-weight:400">· ${esc(sub.path)}${sub.projected ? " · projected" : ""}</span></h2>
<h3>Clone recipes</h3>
${gitlabRecipe ? `<p class="muted">via the GitLab URL (one URL, one credential):</p><pre>${esc(gitlabRecipe)}</pre>` : ""}
<p class="muted">via banyan's own face:</p>
<pre>git clone http://&lt;banyan&gt;/r/${esc(repo)}.git/tree/${esc(segs)}
# work, commit, then push back:
git push origin ${esc(defaultBranch || "main")}</pre>
<h3>Activity <span class="muted" style="font-weight:400">(commits touching ${esc(sub.path)})</span></h3>
<table><tr><th>commit</th><th>subject</th><th>author</th></tr>${actRows || `<tr><td class="muted" colspan="3">no history touching this path yet</td></tr>`}</table>
${rules}
<p class="muted"><a href="${u(`/r/${esc(repo)}/subtrees`)}">&larr; subtrees</a> · <a href="${u(`/r/${esc(repo)}/tree?ref=${encodeURIComponent(defaultBranch || "main")}&path=${encodeURIComponent(segs)}`)}">browse files</a>${top === segs ? "" : ` (top-level projection: ${esc(top)})`}</p>`
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
      const href = u(`/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(child)}`);
      return `<tr><td>${e.isDir ? "&#128193;" : "&#128196;"} <a href="${href}">${esc(e.name)}</a></td><td class="muted">${esc(e.mode)}</td><td class="muted"><code>${esc(e.oid.slice(0, 10))}</code></td></tr>`;
    })
    .join("");
  const crumbs = [`<a href="${u(`/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}`)}">${esc(ref)}:/</a>`]
    .concat(
      path
        .split("/")
        .filter(Boolean)
        .map((seg, i, arr) => {
          const p = arr.slice(0, i + 1).join("/");
          return `<a href="${u(`/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(p)}`)}">${esc(seg)}/</a>`;
        })
    )
    .join(" ");
  return layout(
    `${repo}:${path}`,
    user,
    `<h2>${esc(repo)} <span class="muted" style="font-weight:400">@ ${esc(ref)}</span></h2>
<p class="crumbs">${crumbs}</p>
${parent != null ? `<p><a href="${u(`/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(parent)}`)}">&larr; parent directory</a></p>` : ""}
<table><tr><th>name</th><th>mode</th><th>oid</th></tr>${rows}</table>`
  );
}

export function blobPage(user: string, repo: string, ref: string, path: string, content: string, truncated: boolean): string {
  const parent = path.split("/").slice(0, -1).join("/");
  return layout(
    `${repo}:${path}`,
    user,
    `<h2>${esc(repo)} <span class="muted" style="font-weight:400">@ ${esc(ref)}</span></h2>
<p class="crumbs"><a href="${u(`/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}`)}">${esc(ref)}:/</a> <a href="${u(`/r/${esc(repo)}/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(parent)}`)}">${esc(path)}</a></p>
<pre>${esc(content)}${truncated ? "\n… (truncated for display)" : ""}</pre>`
  );
}

export function errorPage(user: string, status: number, text: string): string {
  return layout("error", user, `<h2>${status}</h2><p>${esc(text)}</p>`);
}
