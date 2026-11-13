// server.ts — the banyan web BFF (backend-for-frontend): a zero-runtime-
// dependency Node service in front of the React console.
//
//   POST /api/login, /api/logout     session lifecycle (JSON)
//   GET  /api/session                who is logged in (SSO mode flag)
//   GET  /api/gitlab-names           sha256(project_id) → project path (SSO display)
//   GET  /api/auth/gitlab            start the OAuth2 authorization-code flow
//   GET  /auth/callback              OAuth2 callback → session → back to the SPA
//   ANY  /api/v1/*                   authenticated reverse proxy to banyan
//                                     (the session's Basic credentials ride
//                                     every call — the SPA never sees them)
//   GET  anything else               static files from dist/ (SPA fallback to
//                                     index.html)
//
// Sessions live in process memory (demo tier): id → credentials. The id
// rides an HttpOnly cookie; credentials never leave the server except as
// the Basic header to banyan. In GitLab-SSO mode the "password" is the
// user's GitLab token — the exact credential shape git uses.
//
// BASE_PATH mounts everything under a URL prefix (GitLab nginx routing
// /-/banyan/ here — full path passthrough, no rewriting).

import * as http from "http";
import * as fs from "fs";
import * as path from "path";
import { URL } from "url";
import * as crypto from "crypto";

const PORT = parseInt(process.env.PORT || "3000", 10);
const BANYAN = process.env.BANYAN_URL || "http://127.0.0.1:8080";
const BASE = (process.env.BASE_PATH || "").replace(/\/+$/, "");
const DIST = process.env.DIST_DIR || path.join(__dirname, "..", "dist");
const cookiePath = BASE === "" ? "/" : BASE;

// ---------------------------------------------------------------------------
// GitLab SSO configuration: env (BANYAN_GITLAB_URL, client id/secret) or the
// file demo provisioning writes (OAUTH_CONFIG_FILE, default
// /config/gitlab-oauth.json). externalUrl is what the BROWSER reaches for the
// authorize redirect; url is what THIS process dials.
interface OAuthCfg {
  url: string;
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

// ---------------------------------------------------------------------------
// Sessions (in-memory, demo tier) and OAuth state anti-CSRF tokens.
interface Session {
  user: string;
  pass: string;
}
const sessions = new Map<string, Session>();
const sessionExpiry = new Map<string, number>();
const SESSION_TTL_MS = 12 * 60 * 60 * 1000;

function newSession(user: string, pass: string): string {
  const id = crypto.randomBytes(24).toString("base64url");
  sessions.set(id, { user, pass });
  sessionExpiry.set(id, Date.now() + SESSION_TTL_MS);
  return id;
}

function sessionOf(req: http.IncomingMessage): Session | null {
  const m = /banyan-sid=([^;]+)/.exec(req.headers.cookie || "");
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

function sessionCookie(id: string): string {
  return `banyan-sid=${id}; HttpOnly; SameSite=Lax; Path=${cookiePath}`;
}

const oauthStates = new Map<string, number>();

// ---------------------------------------------------------------------------
// Small helpers.
function redirect(res: http.ServerResponse, to: string, setCookie?: string): void {
  res.writeHead(302, { Location: to, ...(setCookie ? { "Set-Cookie": setCookie } : {}) });
  res.end();
}

function sendJSON(res: http.ServerResponse, status: number, v: unknown, setCookie?: string): void {
  res.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
    ...(setCookie ? { "Set-Cookie": setCookie } : {}),
  });
  res.end(JSON.stringify(v));
}

function readBody(req: http.IncomingMessage): Promise<Buffer> {
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
  });
}

async function readJSON<T>(req: http.IncomingMessage): Promise<T> {
  const raw = await readBody(req);
  if (raw.length === 0) return {} as T;
  try {
    return JSON.parse(raw.toString("utf8")) as T;
  } catch {
    return {} as T;
  }
}

function basic(sess: Session): string {
  return "Basic " + Buffer.from(`${sess.user}:${sess.pass}`).toString("base64");
}

// GitLab project-name mapping (display only): sha256(project_id) →
// path_with_namespace, fetched with the USER's token (read_api) so it shows
// only what they can see. TTL-cached per token.
const nameCache = new Map<string, { names: Map<string, string>; expires: number }>();

async function gitlabNames(sess: Session): Promise<Map<string, string>> {
  if (!OAUTH) return new Map();
  const key = crypto.createHash("sha256").update(sess.pass).digest("hex");
  const hit = nameCache.get(key);
  if (hit && hit.expires > Date.now()) return hit.names;
  const names = new Map<string, string>();
  try {
    const resp = await fetch(`${OAUTH.url}/api/v4/projects?membership=true&simple=true&per_page=100`, {
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

// ---------------------------------------------------------------------------
// The /api/v1 reverse proxy: same-origin from the SPA's point of view, the
// session's Basic credentials injected server-side. Bodies and responses are
// buffered — the console's API calls are JSON metadata plus bounded blobs.
async function proxyAPI(req: http.IncomingMessage, res: http.ServerResponse, url: URL, sess: Session): Promise<void> {
  const body = await readBody(req);
  const headers: Record<string, string> = { Authorization: basic(sess) };
  const ct = req.headers["content-type"];
  if (ct) headers["Content-Type"] = ct;
  const acc = req.headers.accept;
  if (acc) headers["Accept"] = acc;
  try {
    const upstream = await fetch(BANYAN + url.pathname + url.search, {
      method: req.method,
      headers,
      body: body.length > 0 ? body : undefined,
      redirect: "manual",
    });
    const out = new Map<string, string>();
    for (const h of ["Content-Type", "X-Banyan-Type"]) {
      const v = upstream.headers.get(h);
      if (v) out.set(h, v);
    }
    res.writeHead(upstream.status, Object.fromEntries(out));
    res.end(Buffer.from(await upstream.arrayBuffer()));
  } catch (e) {
    sendJSON(res, 502, { error: `banyan unreachable: ${e instanceof Error ? e.message : String(e)}` });
  }
}

// ---------------------------------------------------------------------------
// Static files from dist/ with an SPA fallback to index.html.
const MIME: Record<string, string> = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".png": "image/png",
  ".svg": "image/svg+xml",
  ".map": "application/json",
  ".txt": "text/plain; charset=utf-8",
  ".woff2": "font/woff2",
};

function serveStatic(res: http.ServerResponse, rel: string): void {
  const file = path.normalize(path.join(DIST, rel));
  if (!file.startsWith(path.normalize(DIST))) {
    res.writeHead(403);
    res.end("forbidden");
    return;
  }
  let data: Buffer;
  try {
    data = fs.readFileSync(file);
  } catch {
    // SPA fallback: every non-API GET is an application route.
    try {
      data = fs.readFileSync(path.join(DIST, "index.html"));
    } catch {
      res.writeHead(500);
      res.end("dist/ missing — run npm run build first");
      return;
    }
  }
  const ext = path.extname(file);
  const immutable = rel.startsWith("assets/");
  res.writeHead(200, {
    "Content-Type": MIME[ext] || "application/octet-stream",
    "Cache-Control": immutable ? "public, max-age=31536000, immutable" : "no-cache",
  });
  res.end(data);
}

// ---------------------------------------------------------------------------
// Request dispatch.
function redirectUri(req: http.IncomingMessage): string {
  const proto = (req.headers["x-forwarded-proto"] as string) || "http";
  const host = req.headers.host || "localhost";
  return `${proto}://${host}${BASE}/auth/callback`;
}

async function handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  const url = new URL(req.url || "/", `http://${req.headers.host || "localhost"}`);
  let pathname: string;
  try {
    pathname = decodeURIComponent(url.pathname);
  } catch {
    pathname = url.pathname;
  }
  if (BASE !== "") {
    if (pathname === BASE) pathname = "/";
    else if (pathname.startsWith(BASE + "/")) pathname = pathname.slice(BASE.length);
    else return serveStatic(res, "index.html");
  }

  // ---- session lifecycle (JSON face for the SPA) ----
  if (pathname === "/api/login" && req.method === "POST") {
    const body = await readJSON<{ user?: string; password?: string }>(req);
    try {
      const resp = await fetch(BANYAN + "/api/v1/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ user: body.user || "", password: body.password || "" }),
      });
      if (!resp.ok) return sendJSON(res, 401, { error: "wrong user or password" });
    } catch (e) {
      return sendJSON(res, 502, { error: `banyan unreachable: ${e instanceof Error ? e.message : String(e)}` });
    }
    const sid = newSession(body.user || "", body.password || "");
    return sendJSON(res, 200, { user: body.user }, sessionCookie(sid));
  }
  if (pathname === "/api/logout" && req.method === "POST") {
    const m = /banyan-sid=([^;]+)/.exec(req.headers.cookie || "");
    if (m) {
      sessions.delete(m[1]);
      sessionExpiry.delete(m[1]);
    }
    return sendJSON(res, 200, { ok: true }, "banyan-sid=; HttpOnly; Max-Age=0; Path=" + cookiePath);
  }
  if (pathname === "/api/mode" && req.method === "GET") {
    // public: which login options the login page should offer
    return sendJSON(res, 200, { sso: !!OAUTH });
  }
  if (pathname === "/api/session" && req.method === "GET") {
    const sess = sessionOf(req);
    if (!sess) return sendJSON(res, 401, { error: "not logged in" });
    return sendJSON(res, 200, { user: sess.user, sso: !!OAUTH });
  }
  if (pathname === "/api/gitlab-names" && req.method === "GET") {
    const sess = sessionOf(req);
    if (!sess) return sendJSON(res, 401, { error: "not logged in" });
    const names = await gitlabNames(sess);
    const out: Record<string, string> = {};
    for (const [k, v] of names) out[k] = v;
    return sendJSON(res, 200, { names: out });
  }

  // ---- GitLab OAuth2 SSO ----
  if (pathname === "/api/auth/gitlab" && req.method === "GET" && OAUTH) {
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
  if (pathname === "/auth/callback" && req.method === "GET" && OAUTH) {
    const code = url.searchParams.get("code") || "";
    const state = url.searchParams.get("state") || "";
    const exp = oauthStates.get(state);
    oauthStates.delete(state);
    const fail = (msg: string, status = 502): void => {
      redirect(res, `${BASE}/login?error=${encodeURIComponent(msg)}`);
      void status;
    };
    if (!code || !exp || exp < Date.now()) return fail("SSO callback rejected (bad or expired state) — try again", 400);
    const form = new URLSearchParams({
      grant_type: "authorization_code",
      client_id: OAUTH.clientId,
      client_secret: OAUTH.clientSecret,
      code,
      redirect_uri: redirectUri(req),
    });
    try {
      const tok = await fetch(OAUTH.url + "/oauth/token", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: form.toString(),
      });
      if (!tok.ok) return fail(`token exchange failed (${tok.status}) — try again`);
      const token = ((await tok.json()) as { access_token?: string }).access_token || "";
      const who = await fetch(OAUTH.url + "/api/v4/user", { headers: { Authorization: `Bearer ${token}` } });
      if (!who.ok || !token) return fail("could not identify the GitLab user — try again");
      const user = ((await who.json()) as { username?: string }).username || "";
      if (!user) return fail("GitLab user has no username?");
      const sid = newSession(user, token);
      return redirect(res, BASE + "/", sessionCookie(sid));
    } catch (e) {
      return fail(`gitlab unreachable: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  // ---- authenticated reverse proxy: everything else under /api/v1 ----
  if (pathname.startsWith("/api/") || (pathname.startsWith("/r/") && pathname.endsWith("/info/refs") && req.method === "GET")) {
    const sess = sessionOf(req);
    if (!sess) return sendJSON(res, 401, { error: "not logged in" });
    return proxyAPI(req, res, url, sess);
  }

  // ---- static SPA (any GET) ----
  if (req.method === "GET" || req.method === "HEAD") {
    return serveStatic(res, pathname.slice(1) || "index.html");
  }
  res.writeHead(405);
  res.end("method not allowed");
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
      `banyan-web (spa+bff) listening on :${PORT} → banyan at ${BANYAN}` +
        (BASE ? ` (base path ${BASE})` : "") +
        (OAUTH ? ` · gitlab sso ${OAUTH.url}` : "")
    );
  });
