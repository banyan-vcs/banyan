// banyan.ts — the server-side client for banyan's RPC and git-wire surfaces
// (docs/10, docs/13 §5). Every call carries the logged-in user's Basic
// credentials, so the SAME authorization rules apply to the web pages as to
// git clients: branch visibility, directory access, everything — the web
// tier never re-implements policy, it forwards identity.

export interface Session {
  user: string;
  pass: string;
}

export interface RepoInfo {
  name: string;
  defaultBranch: string;
}

export interface RefEntry {
  name: string;
  oid: string;
}

export interface LogEntry {
  oid: string;
  subject: string;
  author: string;
  when: number;
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
  isDir: boolean;
}

function basic(sess: Session): string {
  return "Basic " + Buffer.from(`${sess.user}:${sess.pass}`).toString("base64");
}

async function call<T>(base: string, sess: Session, method: string, path: string, body?: unknown): Promise<T> {
  const resp = await fetch(base + path, {
    method,
    headers: {
      Authorization: basic(sess),
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!resp.ok) {
    const text = await resp.text().catch(() => "");
    throw new HttpError(resp.status, text || resp.statusText);
  }
  const ct = resp.headers.get("content-type") || "";
  if (!ct.includes("application/json")) return (await resp.arrayBuffer()) as unknown as T;
  return (await resp.json()) as T;
}

export class HttpError extends Error {
  constructor(public status: number, text: string) {
    super(`${status}: ${text}`);
  }
}

export async function listRepos(base: string, sess: Session): Promise<RepoInfo[]> {
  const out = await call<{ repos: RepoInfo[] }>(base, sess, "GET", "/api/v1/repos");
  return out.repos || [];
}

export async function createRepo(base: string, sess: Session, name: string): Promise<void> {
  await call(base, sess, "POST", "/api/v1/repos", { name });
}

export async function log(base: string, sess: Session, repo: string, limit: number): Promise<LogEntry[]> {
  const out = await call<{ entries: LogEntry[] }>(base, sess, "POST", "/api/v1/log", { repo, limit });
  return out.entries || [];
}

export async function resolve(
  base: string,
  sess: Session,
  repo: string,
  rev: string,
  path: string
): Promise<{ type: string; oid: string; size: number }> {
  return call(base, sess, "POST", "/api/v1/resolve", { repo, rev, path });
}

export async function objectBytes(base: string, sess: Session, repo: string, oid: string): Promise<Buffer> {
  const buf = await call<ArrayBuffer>(base, sess, "GET", `/api/v1/objects?repo=${encodeURIComponent(repo)}&oid=${oid}`);
  return Buffer.from(buf);
}

// listRefs parses the smart-HTTP v1 ref advertisement — the SAME filtered
// advertisement a git client sees, which is what makes branch visibility
// visible in the UI (personal branches of other users simply are not there).
export async function listRefs(base: string, sess: Session, repo: string): Promise<RefEntry[]> {
  const resp = await fetch(`${base}/r/${repo}.git/info/refs?service=git-upload-pack`, {
    headers: { Authorization: basic(sess) },
  });
  if (!resp.ok) throw new HttpError(resp.status, await resp.text().catch(() => resp.statusText));
  const raw = Buffer.from(await resp.arrayBuffer());
  const refs: RefEntry[] = [];
  let pos = 0;
  while (pos + 4 <= raw.length) {
    const n = parseInt(raw.subarray(pos, pos + 4).toString("ascii"), 16);
    if (!n) {
      pos += 4;
      continue; // flush
    }
    if (n < 4 || pos + n > raw.length) break;
    let line = raw.subarray(pos + 4, pos + n).toString("utf8");
    pos += n;
    line = line.replace(/\n$/, "");
    if (line.startsWith("#")) continue; // service header
    const [oid, rest] = line.split(" ", 2);
    if (!rest || !/^[0-9a-f]{40,64}$/.test(oid)) continue;
    const name = rest.split("\0", 1)[0];
    refs.push({ oid, name });
  }
  return refs;
}

// parseTree decodes the git tree object format: entries of
// "<mode> SP <name> NUL <20-byte oid>" (pack-format.txt).
export function parseTree(buf: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
  let pos = 0;
  while (pos < buf.length) {
    const sp = buf.indexOf(" ", pos);
    if (sp < 0) break;
    const nul = buf.indexOf(0, sp + 1);
    if (nul < 0 || nul + 21 > buf.length) break;
    const mode = buf.subarray(pos, sp).toString("ascii");
    const name = buf.subarray(sp + 1, nul).toString("utf8");
    const oid = buf.subarray(nul + 1, nul + 21).toString("hex");
    out.push({ mode, name, oid, isDir: mode === "40000" });
    pos = nul + 21;
  }
  return out;
}
