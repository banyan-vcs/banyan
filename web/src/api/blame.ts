// blame.ts — parse `git blame --line-porcelain` output (what /api/v1/blame
// returns, byte-pinned against git blame) into one row per source line.

export interface BlameLine {
  author: string;
  authorMail: string;
  authorTime: string;
  summary: string;
  oid: string;
  boundary: boolean;
  text: string;
}

export function parseBlame(text: string): BlameLine[] {
  const out: BlameLine[] = [];
  let cur: Partial<BlameLine> & { boundary?: boolean } = {};
  for (const rawLine of text.split("\n")) {
    const line = rawLine.replace(/\r$/, "");
    if (line.startsWith("\t")) {
      out.push({
        author: cur.author || "",
        authorMail: cur.authorMail || "",
        authorTime: cur.authorTime || "",
        summary: cur.summary || "",
        oid: cur.oid || "",
        boundary: !!cur.boundary,
        text: line.slice(1),
      });
      cur = {};
      continue;
    }
    if (line === "boundary") {
      cur.boundary = true;
      continue;
    }
    const sp = line.indexOf(" ");
    if (sp < 0) continue;
    const key = line.slice(0, sp);
    const val = line.slice(sp + 1);
    switch (key) {
      case "author":
        cur.author = val;
        break;
      case "author-mail":
        cur.authorMail = val;
        break;
      case "author-time":
        cur.authorTime = val;
        break;
      case "summary":
        cur.summary = val;
        break;
      default:
        // "<oid> <origLine> <finalLine> <numLines>" header lines: keep the oid
        if (/^[0-9a-f]{40,64}$/.test(key)) cur.oid = key;
        break;
    }
  }
  return out;
}
