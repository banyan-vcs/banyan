// server.ts — the banyan demo web service (docs/07 §5): lightweight
// repository management and file browsing over banyan's RPC + git-wire
// surfaces, with server-side sessions. Zero runtime dependencies.
//
//   GET  /                     repo list (login required)
//   GET/POST /login, /logout   session lifecycle
//   POST /r/new                create repository
//   GET  /r/<repo>             branches (YOUR view) + recent log
//   GET  /r/<repo>/tree        file browser (?ref=&path=)

import * as http from "http";
import { URL } from "url";
import * as crypto from "crypto";
import * as banyan from "./banyan";
import * as render from "./render";

const PORT = parseInt(process.env.PORT || "3000", 10);
const BANYAN = process.env.BANYAN_URL || "http://127.0.0.1:8080";
const BLOB_PREVIEW = 64 * 1024;

// Sessions live in process memory (demo tier): id → credentials. The id
// rides an HttpOnly cookie; credentials never leave the server except as
// the Basic header to banyan — the single place they exist is banyan's
// own auth config.
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

async function handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  const url = new URL(req.url || "/", `http://${req.headers.host || "localhost"}`);
  const path = decodeURIComponent(url.pathname);
  const sess = sessionOf(req);

  try {
    // ---- session lifecycle ----
    if (path === "/login" && req.method === "GET") {
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
      return redirect(res, "/", `banyan-sid=${sid}; HttpOnly; SameSite=Lax; Path=/`);
    }
    if (path === "/logout" && req.method === "POST") {
      const m = /banyan-sid=([^;]+)/.exec(req.headers.cookie || "");
      if (m) {
        sessions.delete(m[1]);
        sessionExpiry.delete(m[1]);
      }
      return redirect(res, "/login", "banyan-sid=; HttpOnly; Max-Age=0; Path=/");
    }
    if (!sess) return redirect(res, "/login");

    // ---- repository management ----
    if (path === "/" && req.method === "GET") {
      const repos = await banyan.listRepos(BANYAN, sess);
      return send(res, render.repoListPage(sess.user, repos, sess.user));
    }
    if (path === "/r/new" && req.method === "POST") {
      const form = await readBody(req);
      try {
        await banyan.createRepo(BANYAN, sess, form.name);
      } catch (e) {
        return send(res, render.errorPage(sess.user, e instanceof banyan.HttpError ? e.status : 500, String(e instanceof Error ? e.message : e)), e instanceof banyan.HttpError ? e.status : 500);
      }
      return redirect(res, `/r/${form.name}`);
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
      return send(res, render.repoPage(sess.user, repo, refs, defaultBranch, log));
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
    console.log(`banyan-web listening on :${PORT} → banyan at ${BANYAN}`);
  });
