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

export interface DiffViewFile {
  oldPath: string;
  newPath: string;
  lang: string;
  hunks: string[];
  binary: boolean;
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
      cur = { oldPath: m ? m[1] : "", newPath: m ? m[2] : "", lang: "plaintext", hunks: [], binary: false };
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
      hunk.push(line.replace(/\r$/, ""));
    }
  }
  flushFile();
  // drop a possible trailing empty line each hunk swallowed from split("\n")
  for (const f of out) f.hunks = f.hunks.map((h) => h.replace(/\n+$/, ""));
  return out.filter((f) => f.hunks.length > 0 || f.binary);
}
