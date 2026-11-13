// diff.ts — parse a unified diff (what /api/v1/diff returns, byte-pinned
// against git diff) into renderable rows.

export interface DiffRow {
  kind: "context" | "add" | "del" | "hunk" | "meta";
  oldLine: number | null;
  newLine: number | null;
  text: string;
}

export interface DiffFile {
  oldPath: string;
  newPath: string;
  rows: DiffRow[];
}

export function parseUnifiedDiff(text: string): DiffFile[] {
  const files: DiffFile[] = [];
  let cur: DiffFile | null = null;
  let oldLine = 0;
  let newLine = 0;
  const body = text.endsWith("\n") ? text.slice(0, -1) : text;
  for (const rawLine of body.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    if (line.startsWith("diff --git ")) {
      cur = { oldPath: "", newPath: "", rows: [] };
      files.push(cur);
      const m = /a\/(\S+) b\/(\S+)$/.exec(line);
      if (m) {
        cur.oldPath = m[1];
        cur.newPath = m[2];
      }
      cur.rows.push({ kind: "meta", oldLine: null, newLine: null, text: line });
      continue;
    }
    if (!cur) continue; // preamble before the first diff --git (e.g. commit id)
    if (line.startsWith("--- ") || line.startsWith("+++ ") || line.startsWith("index ") || line.startsWith("new file") || line.startsWith("deleted file") || line.startsWith("old mode") || line.startsWith("new mode") || line.startsWith("similarity") || line.startsWith("rename ") || line.startsWith("copy ") || line.startsWith("Binary files")) {
      cur.rows.push({ kind: "meta", oldLine: null, newLine: null, text: line });
      if (line.startsWith("+++ ")) cur.newPath = line.slice(4).replace(/^b\//, "");
      if (line.startsWith("--- ")) cur.oldPath = line.slice(4).replace(/^a\//, "");
      continue;
    }
    const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
    if (hunk) {
      oldLine = parseInt(hunk[1], 10);
      newLine = parseInt(hunk[2], 10);
      cur.rows.push({ kind: "hunk", oldLine: null, newLine: null, text: line });
      continue;
    }
    if (line.startsWith("+")) {
      cur.rows.push({ kind: "add", oldLine: null, newLine: newLine++, text: line.slice(1) });
    } else if (line.startsWith("-")) {
      cur.rows.push({ kind: "del", oldLine: oldLine++, newLine: null, text: line.slice(1) });
    } else if (line.startsWith(" ") || line === "") {
      cur.rows.push({ kind: "context", oldLine: oldLine++, newLine: newLine++, text: line.replace(/^ /, "") });
    } else if (line.startsWith("\\")) {
      cur.rows.push({ kind: "meta", oldLine: null, newLine: null, text: line });
    }
  }
  return files;
}

export function diffStat(files: DiffFile[]): { add: number; del: number } {
  let add = 0;
  let del = 0;
  for (const f of files) {
    for (const r of f.rows) {
      if (r.kind === "add") add++;
      else if (r.kind === "del") del++;
    }
  }
  return { add, del };
}
