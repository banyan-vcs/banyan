// server.ts — the banyan web service (docs/07 §5, docs/20 子树管理控制面):
// lightweight repository management, file browsing and SUBTREE MANAGEMENT
// over banyan's RPC + git-wire surfaces, with server-side sessions. Zero
// runtime dependencies.
//
//   GET  /                     repo list (login required)
//   GET/POST /login, /logout   session lifecycle
//   GET  /auth/callback        GitLab OAuth2 SSO callback (when configured)
//   POST /r/new                create repository
//   GET  /r/<repo>             branches (YOUR view) + recent log
//   GET  /r/<repo>/tree        file browser (?ref=&path=)
//   GET  /r/<repo>/subtrees    subtree management listing (docs/07 §8c)
//   GET  /r/<repo>/subtree     one subtree: activity + recipes + rule form
//   POST /r/<repo>/subtree-access[/delete]  the rule form targets
//
// Two auth modes. Local (default): the login form checks banyan's own
// /api/v1/auth/login and the session holds the user's Basic credentials.
// GitLab SSO (BANYAN_GITLAB_URL + client id/secret, or /config/gitlab-
// oauth.json): the login button starts an OAuth2 authorization-code flow
// against GitLab (read_api); the session's "password" is the resulting
// GitLab token, which banyan's GitLabSSO policy validates — the exact
// credential shape git uses.
//
// BASE_PATH mounts everything under a URL prefix (e.g. GitLab nginx routing
// /-/banyan/ here — full path passthrough, no rewriting).

import * as http from "http";
import * as fs from "fs";
import { URL } from "url";
import * as crypto from "crypto";
import * as banyan from "./banyan";
import * as render from "./render";
import { AVATAR_PNG } from "./avatar";

const PORT = parseInt(process.env.PORT || "3000", 10);
const BANYAN = process.env.BANYAN_URL || "http://127.0.0.1:8080";
const BASE = (process.env.BASE_PATH || "").replace(/\/+$/, "");
const BLOB_PREVIEW = 64 * 1024;
render.setBase(BASE);

// OAuthCfg is the GitLab SSO configuration: from env (BANYAN_GITLAB_URL,
// BANYAN_GITLAB_CLIENT_ID, BANYAN_GITLAB_CLIENT_SECRET) or the file demo
// provisioning writes (OAUTH_CONFIG_FILE, default /config/gitlab-oauth.json
// — the provision script mints the application and drops the file here).
interface OAuthCfg {
  url: string;
  // externalUrl is what the BROWSER must reach for the authorize redirect
  // (the public GitLab URL); url is what THIS process dials (token exchange,
  // /api/v4). Same by default — they differ when the web tier sits on the
  // docker network next to GitLab (url=http://gitlab:8180,
  // externalUrl=http://gitlab.example.com:8180).
  externalUrl: string;
  clientId: string;
  clientSecret: string;
}

function loadOAuth(): OAuthCfg | null {
  const url = process.env.BANYAN_GITLAB_URL;
  const id = process.env.BANYAN_GITLAB_CLIENT_ID;
  const sec = process.env.BANYAN_GITLAB_CLIENT_SECRET;
  const ext = process.env.BANYAN_GITLAB_EXTERNAL_URL || url;
  if (url && id && sec) {
    return { url: url.replace(/\/+$/, ""), externalUrl: (ext || url).replace(/\/+$/, ""), clientId: id, clientSecret: sec };
  }
  const file = process.env.OAUTH_CONFIG_FILE || "/config/gitlab-oauth.json";
  try {
    const j = JSON.parse(fs.readFileSync(file, "utf8"));
    if (j.url && j.client_id && j.client_secret) {
      return {
        url: String(j.url).replace(/\/+$/, ""),
        externalUrl: String(j.external_url || j.url).replace(/\/+$/, ""),
        clientId: String(j.client_id),
        clientSecret: String(j.client_secret),
      };
    }
  } catch {
    /* no config file — local auth mode */
  }
  return null;
}
const OAUTH = loadOAuth();

// Sessions live in process memory (demo tier): id → credentials. The id
// rides an HttpOnly cookie; credentials never leave the server except as
// the Basic header to banyan — the single place they exist is banyan's
// own auth plane (the static table, or GitLab when SSO is on).
const sessions = new Map<string, banyan.Session>();
const SESSION_TTL_MS = 12 * 60 * 60 * 1000;
const sessionExpiry = new Map<string, number>();

function newSession(user: string, pass: string): string {
  const id = crypto.randomBytes(24).toString("base64url");
  sessions.set(id, { user, pass });
  sessionExpiry.set(id, Date.now() + SESSION_TTL_MS);
  return id;
}

function sessionOf(req: http.IncomingMessage): banyan.Session | null {
  const cookie = req.headers.cookie || "";
  const m = /banyan-sid=([^;]+)/.exec(cookie);
  if (!m) return null;
  const id = m[1];
  const exp = sessionExpiry.get(id);
  if (exp === undefined || exp < Date.now()) {
    sessions.delete(id);
    sessionExpiry.delete(id);
    return null;
  }
  return sessions.get(id) || null;
}

// OAuth state anti-CSRF tokens: state → expiry.
const oauthStates = new Map<string, number>();

function redirectUri(req: http.IncomingMessage): string {
  const proto = (req.headers["x-forwarded-proto"] as string) || "http";
  const host = req.headers.host || "localhost";
  return `${proto}://${host}${render.u("/auth/callback")}`;
}

function readBody(req: http.IncomingMessage): Promise<Record<string, string>> {
  return new Promise((resolve) => {
    let data = "";
    req.on("data", (c) => (data += c));
    req.on("end", () => {
      const out: Record<string, string> = {};
      for (const [k, v] of new URLSearchParams(data)) out[k] = v;
      resolve(out);
    });
  });
}

function send(res: http.ServerResponse, html: string, status = 200, headers: Record<string, string> = {}): void {
  res.writeHead(status, { "Content-Type": "text/html; charset=utf-8", ...headers });
  res.end(html);
}

function redirect(res: http.ServerResponse, to: string, setCookie?: string): void {
  res.writeHead(302, { Location: to, ...(setCookie ? { "Set-Cookie": setCookie } : {}) });
  res.end();
}

const cookiePath = BASE === "" ? "/" : BASE;

// GitLab project-name mapping (display only): sha256(project_id) →
// path_with_namespace, fetched with the USER's token (read_api) so the
// mapping naturally shows only what they can see. TTL-cached per token.
const nameCache = new Map<string, { names: Map<string, string>; expires: number }>();

async function gitlabNames(sess: banyan.Session): Promise<Map<string, string>> {
  if (!OAUTH) return new Map();
  const key = crypto.createHash("sha256").update(sess.pass).digest("hex");
  const hit = nameCache.get(key);
  if (hit && hit.expires > Date.now()) return hit.names;
  const names = new Map<string, string>();
  try {
    const resp = await fetch(`${OAUTH.url}/api/v4/projects?membership=true&simple=true&per_page=100&pagination=keyset`, {
      headers: { Authorization: `Bearer ${sess.pass}` },
    });
    if (resp.ok) {
      const projects = (await resp.json()) as { id: number; path_with_namespace: string }[];
      for (const p of projects) {
        names.set(crypto.createHash("sha256").update(String(p.id)).digest("hex"), p.path_with_namespace);
      }
    }
  } catch {
    /* GitLab unreachable: fall back to raw repo ids */
  }
  nameCache.set(key, { names, expires: Date.now() + 60_000 });
  return names;
}

// gitlabRecipe renders the projection-ref clone recipe when the repo maps
// to a GitLab project (one URL, one credential — docs/20 §7).
function gitlabRecipe(display: string, top: string): string | null {
  if (!display || !display.includes("/")) return null;
  return [
    `git clone http://<gitlab-host>/${display}.git`,
    `git fetch origin refs/banyan/tree/${top}:refs/heads/${top}`,
    `git checkout ${top}                    # workspace = ${top}/, history = ${top}/ only`,
    `# work, commit, then push back:`,
    `git push origin ${top}:refs/banyan/tree/${top}`,
  ].join("\n");
}

async function handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  const url = new URL(req.url || "/", `http://${req.headers.host || "localhost"}`);
  let path = decodeURIComponent(url.pathname);
  if (BASE !== "") {
    // Everything the nginx location routes here carries the base prefix
    // (full-path passthrough). Strip it once; the route table below stays
    // app-relative.
    if (path === BASE) path = "/";
    else if (path.startsWith(BASE + "/")) path = path.slice(BASE.length);
    else return send(res, render.errorPage("", 404, `not under ${BASE}: ${path}`), 404);
  }
  const sess = sessionOf(req);

  try {
    // ---- static: the project avatar (favicon + header brand) ----
    if (path === "/avatar.png") {
      res.writeHead(200, {
        "Content-Type": "image/png",
        "Cache-Control": "public, max-age=86400",
      });
      res.end(AVATAR_PNG);
      return;
    }

    // ---- session lifecycle ----
    if (path === "/login" && req.method === "GET") {
      if (OAUTH) {
        // GitLab SSO: off to the authorize endpoint with a fresh state.
        const state = crypto.randomBytes(16).toString("hex");
        oauthStates.set(state, Date.now() + 10 * 60 * 1000);
        const q = new URLSearchParams({
          client_id: OAUTH.clientId,
          redirect_uri: redirectUri(req),
          response_type: "code",
          scope: "read_api",
          state,
        });
        return redirect(res, `${OAUTH.externalUrl}/oauth/authorize?${q.toString()}`);
      }
      return send(res, render.loginPage());
    }
    if (path === "/login" && req.method === "POST") {
      const form = await readBody(req);
      try {
        const resp = await fetch(BANYAN + "/api/v1/auth/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ user: form.user || "", password: form.password || "" }),
        });
        if (!resp.ok) return send(res, render.loginPage("wrong user or password"));
      } catch (e) {
        return send(res, render.loginPage(`banyan unreachable: ${e instanceof Error ? e.message : e}`), 502);
      }
      const sid = newSession(form.user, form.password || "");
      return redirect(res, render.u("/"), `banyan-sid=${sid}; HttpOnly; SameSite=Lax; Path=${cookiePath}`);
    }
    if (path === "/auth/callback" && req.method === "GET" && OAUTH) {
      const code = url.searchParams.get("code") || "";
      const state = url.searchParams.get("state") || "";
      const exp = oauthStates.get(state);
      oauthStates.delete(state);
      if (!code || !exp || exp < Date.now()) {
        return send(res, render.loginPage("SSO callback rejected (bad or expired state) — try again"), 400);
      }
      // Exchange the code, then identify the user — both against GitLab.
      const form = new URLSearchParams({
        grant_type: "authorization_code",
        client_id: OAUTH.clientId,
        client_secret: OAUTH.clientSecret,
        code,
        redirect_uri: redirectUri(req),
      });
      const tok = await fetch(`${OAUTH.url}/oauth/token`, {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: form.toString(),
      });
      if (!tok.ok) {
        return send(res, render.loginPage(`token exchange failed (${tok.status}) — try again`), 502);
      }
      const token = ((await tok.json()) as { access_token?: string }).access_token || "";
      const who = await fetch(`${OAUTH.url}/api/v4/user`, { headers: { Authorization: `Bearer ${token}` } });
      if (!who.ok || !token) {
        return send(res, render.loginPage("could not identify the GitLab user — try again"), 502);
      }
      const user = ((await who.json()) as { username?: string }).username || "";
      if (!user) return send(res, render.loginPage("GitLab user has no username?"), 502);
      const sid = newSession(user, token);
      return redirect(res, render.u("/"), `banyan-sid=${sid}; HttpOnly; SameSite=Lax; Path=${cookiePath}`);
    }
    if (path === "/logout" && req.method === "POST") {
      const m = /banyan-sid=([^;]+)/.exec(req.headers.cookie || "");
      if (m) {
        sessions.delete(m[1]);
        sessionExpiry.delete(m[1]);
      }
      return redirect(res, render.u("/login"), `banyan-sid=; HttpOnly; Max-Age=0; Path=${cookiePath}`);
    }
    if (!sess) return redirect(res, render.u("/login"));

    // ---- repository management ----
    if (path === "/" && req.method === "GET") {
      const repos = await banyan.listRepos(BANYAN, sess);
      const names = await gitlabNames(sess);
      const display = names.size ? (id: string) => names.get(id) || "" : undefined;
      return send(res, render.repoListPage(sess.user, repos, sess.user, display));
    }
    if (path === "/r/new" && req.method === "POST") {
      const form = await readBody(req);
      try {
        await banyan.createRepo(BANYAN, sess, form.name);
      } catch (e) {
        return send(res, render.errorPage(sess.user, e instanceof banyan.HttpError ? e.status : 500, String(e instanceof Error ? e.message : e)), e instanceof banyan.HttpError ? e.status : 500);
      }
      return redirect(res, render.u(`/r/${form.name}`));
    }

    // ---- repo pages ----
    const mt = /^\/r\/([^/]+)\/tree$/.exec(path);
    if (mt) {
      const repo = mt[1];
      const ref = url.searchParams.get("ref") || "";
      const treePath = (url.searchParams.get("path") || "").replace(/^\/+|\/+$/g, "");
      const resolved = await banyan.resolve(BANYAN, sess, repo, ref, treePath);
      if (resolved.type === "tree") {
        const raw = await banyan.objectBytes(BANYAN, sess, repo, resolved.oid);
        const entries = banyan.parseTree(raw).sort((a, b) => (a.isDir === b.isDir ? a.name.localeCompare(b.name) : a.isDir ? -1 : 1));
        const parent = treePath ? treePath.split("/").slice(0, -1).join("/") : null;
        return send(res, render.treePage(sess.user, repo, ref || "main", treePath, entries, parent));
      }
      // blob preview
      const raw = await banyan.objectBytes(BANYAN, sess, repo, resolved.oid);
      const truncated = raw.length > BLOB_PREVIEW;
      const text = raw.subarray(0, BLOB_PREVIEW).toString("utf8");
      return send(res, render.blobPage(sess.user, repo, ref || "main", treePath, text, truncated));
    }

    // ---- subtree management (docs/07 §8c, docs/20) ----
    const msub = /^\/r\/([^/]+)\/subtrees$/.exec(path);
    if (msub) {
      const repo = msub[1];
      const listing = await banyan.subtrees(BANYAN, sess, repo);
      let rules: banyan.SubtreeRule[] | null = null;
      try {
        rules = await banyan.subtreeRules(BANYAN, sess, repo);
      } catch (e) {
        if (!(e instanceof banyan.HttpError && e.status === 403)) throw e; // non-admin: no management view
      }
      const names = await gitlabNames(sess);
      return send(res, render.subtreesPage(sess.user, repo, listing, rules, names.get(repo) || ""));
    }
    const msdone = /^\/r\/([^/]+)\/subtree$/.exec(path);
    if (msdone) {
      const repo = msdone[1];
      const subPath = url.searchParams.get("path") || "";
      const listing = await banyan.subtrees(BANYAN, sess, repo);
      const sub = listing.subtrees.find((s) => s.path === subPath);
      if (!sub) {
        return send(res, render.errorPage(sess.user, 404, `no subtree timeline at ${subPath}`), 404);
      }
      let rules: banyan.SubtreeRule[] | null = null;
      try {
        rules = await banyan.subtreeRules(BANYAN, sess, repo);
      } catch (e) {
        if (!(e instanceof banyan.HttpError && e.status === 403)) throw e;
      }
      const rule = (rules || []).find((r) => r.path === subPath) || null;
      let activity: banyan.LogEntry[] = [];
      try {
        activity = await banyan.log(BANYAN, sess, repo, 20, subPath);
      } catch {
        /* no history at this path */
      }
      const names = await gitlabNames(sess);
      const display = names.get(repo) || "";
      const top = subPath.replace(/^\//, "").split("/")[0];
      return send(res, render.subtreeDetailPage(sess.user, repo, sub, listing.defaultBranch, activity, gitlabRecipe(display, top), rule, rules !== null, display));
    }
    const mrule = /^\/r\/([^/]+)\/subtree-access$/.exec(path);
    if (mrule && req.method === "POST") {
      const repo = mrule[1];
      const form = await readBody(req);
      const users = (form.users || "").split(/[\s,]+/).filter(Boolean);
      await banyan.putSubtreeRule(BANYAN, sess, repo, form.path || "", users);
      return redirect(res, render.u(`/r/${repo}/subtree?path=${encodeURIComponent(form.path || "")}`));
    }
    const mruleDel = /^\/r\/([^/]+)\/subtree-access\/delete$/.exec(path);
    if (mruleDel && req.method === "POST") {
      const repo = mruleDel[1];
      const form = await readBody(req);
      await banyan.deleteSubtreeRule(BANYAN, sess, repo, form.path || "");
      return redirect(res, render.u(`/r/${repo}/subtree?path=${encodeURIComponent(form.path || "")}`));
    }

    const mr = /^\/r\/([^/]+)$/.exec(path);
    if (mr) {
      const repo = mr[1];
      const refs = await banyan.listRefs(BANYAN, sess, repo);
      const head = refs.find((r) => r.name === "HEAD");
      const defaultBranch = head ? "main" : "main";
      let log: banyan.LogEntry[] = [];
      try {
        const branch = (refs.find((r) => r.name === "refs/heads/main") || refs.find((r) => r.name.startsWith("refs/heads/")))?.oid || "";
        if (branch) log = await banyan.log(BANYAN, sess, repo, 10);
      } catch {
        /* empty repo: no log yet */
      }
      const names = await gitlabNames(sess);
      return send(res, render.repoPage(sess.user, repo, refs, defaultBranch, log, names.get(repo) || ""));
    }

    return send(res, render.errorPage(sess.user, 404, `no such page: ${path}`), 404);
  } catch (e) {
    // sess narrowed above; keep a non-null copy for the error path.
    const who = sess ? sess.user : "";
    if (e instanceof banyan.HttpError) {
      if (e.status === 401) return send(res, render.loginPage("session rejected — log in again"));
      return send(res, render.errorPage(who, e.status, e.message), e.status);
    }
    return send(res, render.errorPage(who, 502, `banyan error: ${e instanceof Error ? e.message : String(e)}`), 502);
  }
}

http
  .createServer((req, res) => {
    handle(req, res).catch((e) => {
      res.writeHead(500);
      res.end(`internal error: ${e instanceof Error ? e.message : e}`);
    });
  })
  .listen(PORT, () => {
    console.log(
      `banyan-web listening on :${PORT} → banyan at ${BANYAN}` +
        (BASE ? ` (base path ${BASE})` : "") +
        (OAUTH ? ` · gitlab sso ${OAUTH.url}` : "")
    );
  });
