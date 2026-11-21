// readme.ts — the repository README contract: which root file counts as
// the README (README.md first, README.markdown next, case-insensitive),
// how to fetch its text, and how README-relative image/link paths map
// onto this console's URLs (the raw face for images, the tree route for
// links). The path rewriting is pure — the rendering tests pin it.
import { api, qs, BASE_URL } from "./client";
import type { TreeResponse } from "./types";

export interface ReadmeContent {
  name: string;
  text: string;
}

/** pickReadme answers the README file among a directory's names, or null.
 * README.md wins over README.markdown (the order repositories actually
 * carry them), any casing counts, and same-rank ties resolve
 * lexicographically for determinism. */
export function pickReadme(names: string[]): string | null {
  const rank = (n: string): number => {
    const lower = n.toLowerCase();
    if (lower === "readme.md") return 0;
    if (lower === "readme.markdown") return 1;
    return 2;
  };
  let best: string | null = null;
  let bestRank = 2;
  for (const n of names) {
    const r = rank(n);
    if (r >= 2) continue;
    if (best === null || r < bestRank || (r === bestRank && n < best)) {
      best = n;
      bestRank = r;
    }
  }
  return best;
}

/** fetchReadme loads the repo's README text at rev ("" = the default
 * branch), or null when the root carries none. One tree call answers the
 * candidate's oid (entries carry it); one objects call answers the bytes. */
export async function fetchReadme(repo: string, rev: string): Promise<ReadmeContent | null> {
  const tree = await api<TreeResponse>(`/api/v1/tree${qs({ repo, rev })}`);
  const name = pickReadme(tree.entries.filter((e) => e.type === "blob").map((e) => e.name));
  const entry = name ? tree.entries.find((e) => e.name === name) : undefined;
  if (!entry || entry.type !== "blob") return null;
  const buf = await api<ArrayBuffer>(`/api/v1/objects${qs({ repo, oid: entry.oid })}`);
  return { name: entry.name, text: new TextDecoder().decode(buf) };
}

/** normalizeRepoPath resolves a README-relative reference against the
 * README's directory: "./" and redundant segments collapse, ".." pops, a
 * leading "/" means the repo root, and an escape past the root answers
 * null. The tail (any "?query#frag"/"#frag") rides along untouched. */
export function normalizeRepoPath(src: string, dir: string): { segs: string[]; tail: string } | null {
  const cut = (s: string, seps: string): [string, string] => {
    let at = -1;
    for (const c of seps) {
      const i = s.indexOf(c);
      if (i !== -1 && (at === -1 || i < at)) at = i;
    }
    return at === -1 ? [s, ""] : [s.slice(0, at), s.slice(at)];
  };
  const [p, tail] = cut(src, "?#");
  const out: string[] = [];
  for (const seg of (dir === "" || p.startsWith("/") ? [] : dir.split("/")).concat(p.split("/"))) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") {
      if (out.length === 0) return null;
      out.pop();
      continue;
    }
    out.push(seg);
  }
  if (out.length === 0) return null;
  return { segs: out, tail };
}

/** repoAssetURL answers the <img> src for a README image reference:
 * passthrough for http(s)/data:image/blob, null for anything that is not
 * a repo path (javascript: and friends die here), and the raw face for
 * relative and repo-root references. */
export function repoAssetURL(src: string, dir: string, repo: string, rev: string): string | null {
  if (/^(https?:|data:image\/|blob:)/i.test(src)) return src;
  if (src.startsWith("#")) return src;
  if (/^[a-z][a-z0-9+.-]*:/i.test(src)) return null;
  const norm = normalizeRepoPath(src, dir);
  if (!norm) return null;
  return (
    `${BASE_URL}api/v1/repos/${encodeURIComponent(repo)}/raw/` +
    norm.segs.map(encodeURIComponent).join("/") +
    (rev ? `?rev=${encodeURIComponent(rev)}` : "") +
    norm.tail
  );
}
