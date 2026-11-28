// diffview.ts — adapt the gateway's unified diff text (byte-pinned against
// git diff) to @git-diff-view's Git Diff Mode: per-file hunk strings plus
// the lowlight language id derived from the path.
//
// Hunk strings are verbatim blocks of the raw diff: the @@ header through
// the last line before the next @@ or file boundary. The component pairs
// them with (possibly empty) whole-file contents for word-diff; without
// contents it still numbers and highlights the hunks' own lines.

const LANGS: Record<string, string> = {
  go: "go", ts: "typescript", tsx: "tsx", js: "javascript", jsx: "jsx",
  py: "python", rb: "ruby", rs: "rust", java: "java", kt: "kotlin",
  c: "c", h: "c", cc: "cpp", cpp: "cpp", hpp: "cpp", cs: "csharp",
  sh: "bash", bash: "bash", yaml: "yaml", yml: "yaml", json: "json",
  md: "markdown", sql: "sql", css: "css", scss: "scss", less: "less",
  html: "xml", xml: "xml", toml: "ini", ini: "ini", proto: "protobuf",
  php: "php", lua: "lua", swift: "swift", r: "r", txt: "plaintext",
};

export function langOf(path: string): string {
  const base = path.split("/").pop() || path;
  if (base.toLowerCase() === "dockerfile" || base.toLowerCase().startsWith("dockerfile.")) return "dockerfile";
  if (base.toLowerCase() === "makefile") return "makefile";
  const dot = base.lastIndexOf(".");
  if (dot < 0) return "plaintext";
  return LANGS[base.slice(dot + 1).toLowerCase()] || "plaintext";
}

/** The localStorage key both diff surfaces (commit, merge request) remember the
 * split/unified choice under — one preference follows the user across
 * pages. */
export const DIFF_VIEW_KEY = "banyan.diff.view";

export interface DiffViewFile {
  oldPath: string;
  newPath: string;
  lang: string;
  hunks: string[];
  binary: boolean;
  add: number; // + lines across all hunks (0 for binary)
  del: number; // − lines across all hunks
}

/** Split a unified diff into per-file hunk blocks for the diff view. */
export function toDiffViewFiles(text: string): DiffViewFile[] {
  const out: DiffViewFile[] = [];
  let cur: DiffViewFile | null = null;
  let hunk: string[] | null = null;
  const flushHunk = (): void => {
    if (cur && hunk && hunk.length > 0) cur.hunks.push(hunk.join("\n"));
    hunk = null;
  };
  const flushFile = (): void => {
    flushHunk();
    if (cur && (cur.hunks.length > 0 || cur.binary)) out.push(cur);
    cur = null;
  };
  for (const line of text.split("\n")) {
    if (line.startsWith("diff --git ")) {
      flushFile();
      const m = / a\/(\S+) b\/(\S+)$/.exec(line);
      cur = { oldPath: m ? m[1] : "", newPath: m ? m[2] : "", lang: "plaintext", hunks: [], binary: false, add: 0, del: 0 };
      continue;
    }
    if (!cur) continue; // preamble (commit id, etc.)
    if (line.startsWith("Binary files")) {
      cur.binary = true;
      continue;
    }
    if (line.startsWith("--- ")) {
      cur.oldPath = line.slice(4).replace(/^a\//, "").replace(/^\.\//, "");
      continue;
    }
    if (line.startsWith("+++ ")) {
      cur.newPath = line.slice(4).replace(/^b\//, "").replace(/^\.\//, "");
      cur.lang = langOf(cur.newPath || cur.oldPath);
      continue;
    }
    if (line.startsWith("@@")) {
      flushHunk();
      hunk = [line];
      continue;
    }
    if (hunk !== null) {
      // stop swallowing trailing file-level lines after the last hunk
      const l = line.replace(/\r$/, "");
      hunk.push(l);
      // the per-file diffstat rides the same walk (body lines carry the
      // +/- sigil; "\ No newline" markers start with a backslash)
      if (l.startsWith("+")) cur.add++;
      else if (l.startsWith("-")) cur.del++;
    }
  }
  flushFile();
  // Each hunk string must carry its file header back: @git-diff-view's
  // parser (GitHub Desktop lineage) only recognizes a @@ block once it has
  // seen the ---/+++ pair — a bare @@ block is swallowed as header text
  // and parses to ZERO hunks (the empty-diff bug). /dev/null rides the
  // standard new/delete spellings. A trailing newline closes the last line.
  const headerFor = (f: DiffViewFile): string => {
    const oldP = !f.oldPath || f.oldPath === "/dev/null" ? "/dev/null" : `a/${f.oldPath}`;
    const newP = !f.newPath || f.newPath === "/dev/null" ? "/dev/null" : `b/${f.newPath}`;
    return `--- ${oldP}\n+++ ${newP}\n`;
  };
  for (const f of out) f.hunks = f.hunks.map((h) => headerFor(f) + h.replace(/\n+$/, "") + "\n");
  return out.filter((f) => f.hunks.length > 0 || f.binary);
}
