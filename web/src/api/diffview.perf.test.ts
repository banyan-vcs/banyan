// diffview.perf.test.ts — a locally-gated bench, not a CI test: it only
// wakes when DIFF_BENCH_FILE points at a real unified diff (e.g. the
// 127-file banyan-web.patch corpus from `git diff A..B`). It prints the
// phase costs of the pipeline the browser pays on every commit/merge request page —
// adapter parse, core initRaw (hunk parse + content compose), initSyntax
// (lowlight over the composed file), and the split/unified row builders —
// so rendering regressions have numbers, not anecdotes. With
// DIFF_OLD_REV/DIFF_NEW_REV it also times the same file highlighted from
// real whole-file contents instead of the hunk-composed guess.
import { execSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { DiffFile } from "@git-diff-view/core";
import { toDiffViewFiles } from "./diffview";

const benchFile = process.env.DIFF_BENCH_FILE;

describe.skipIf(!benchFile)("diff pipeline bench (local only)", () => {
  it("reports phase timings for the corpus", () => {
    const text = readFileSync(benchFile!, "utf8");
    const ms = (fn: () => void): number => {
      const t0 = performance.now();
      fn();
      return performance.now() - t0;
    };

    let files: ReturnType<typeof toDiffViewFiles> = [];
    const tAdapter = ms(() => (files = toDiffViewFiles(text)));

    let raw = 0;
    let syntax = 0;
    let build = 0;
    let rows = 0;
    const perFile: { path: string; lang: string; syntax: number; rows: number }[] = [];
    for (const f of files) {
      if (f.binary) continue;
      const df = new DiffFile(f.oldPath, "", f.newPath, "", f.hunks, f.lang, f.lang);
      raw += ms(() => df.initRaw());
      let one = 0;
      one += ms(() => df.initSyntax());
      syntax += one;
      build += ms(() => {
        df.buildSplitDiffLines();
        df.buildUnifiedDiffLines();
      });
      rows += df.unifiedLineLength;
      perFile.push({ path: f.newPath, lang: f.lang, syntax: one, rows: df.unifiedLineLength });
      if (f === files.find((x) => x.lang === "go")) {
        const line = df.getNewSyntaxLine(1);
        expect(line?.nodeList?.length ?? 0, "initSyntax must produce syntax nodes").toBeGreaterThan(0);
      }
    }

    console.info(
      `[bench] files=${files.length} hunks=${files.reduce((n, f) => n + f.hunks.length, 0)} ` +
        `add=${files.reduce((n, f) => n + f.add, 0)} del=${files.reduce((n, f) => n + f.del, 0)} unifiedRows=${rows}`
    );
    console.info(
      `[bench] adapter=${tAdapter.toFixed(0)}ms initRaw=${raw.toFixed(0)}ms initSyntax=${syntax.toFixed(0)}ms ` +
        `build=${build.toFixed(0)}ms total=${(tAdapter + raw + syntax + build).toFixed(0)}ms`
    );
    console.info(
      "[bench] slowest highlight:",
      perFile
        .slice()
        .sort((a, b) => b.syntax - a.syntax)
        .slice(0, 5)
        .map((p) => `${p.path}(${p.lang}, ${p.rows}r, ${p.syntax.toFixed(0)}ms)`)
        .join("  ")
    );

    // real whole-file contents: what fetching old/new blobs would cost the
    // highlighter (and what fidelity the composed guess gives up)
    const oldRev = process.env.DIFF_OLD_REV;
    const newRev = process.env.DIFF_NEW_REV;
    if (oldRev && newRev) {
      let full = 0;
      let n = 0;
      for (const f of files) {
        if (f.binary || f.oldPath === "/dev/null" || f.newPath === "/dev/null") continue;
        const oldC = execSync(`git show "${oldRev}:${f.oldPath}"`, { maxBuffer: 1 << 26 }).toString();
        const newC = execSync(`git show "${newRev}:${f.newPath}"`, { maxBuffer: 1 << 26 }).toString();
        const df = new DiffFile(f.oldPath, oldC, f.newPath, newC, f.hunks, f.lang, f.lang);
        full += ms(() => {
          df.initRaw();
          df.initSyntax();
          df.buildSplitDiffLines();
          df.buildUnifiedDiffLines();
        });
        n++;
      }
      console.info(`[bench] full-content pipeline over ${n} files: ${full.toFixed(0)}ms`);
    }
    expect(rows).toBeGreaterThan(0);
  });
});
