// diffview.test.ts — the adapter is load-bearing (the merge request diff view rides
// it), so the hunk extraction and language mapping are pinned against a
// hand-checked two-file diff. The DiffFile integration test pins the
// contract @git-diff-view actually parses: a hunk string without its
// ---/+++ header parses to ZERO hunks (the empty-diff bug).
import { describe, expect, it } from "vitest";
import { DiffFile } from "@git-diff-view/core";
import { langOf, toDiffViewFiles } from "./diffview";

const sample = `commit abc
diff --git a/main.go b/main.go
index 111..222 100644
--- a/main.go
+++ b/main.go
@@ -1,4 +1,5 @@
 package main
+import "fmt"
 
 func main() {
 }
diff --git a/web/src/x.tsx b/web/src/x.tsx
new file mode 100644
--- /dev/null
+++ b/web/src/x.tsx
@@ -0,0 +1,2 @@
+export const A = 1;
+export const B = 2;
diff --git a/logo.png b/logo.png
Binary files a/logo.png and b/logo.png differ
`;

describe("toDiffViewFiles", () => {
  it("extracts one entry per file with verbatim hunk blocks", () => {
    const files = toDiffViewFiles(sample);
    expect(files.map((f) => f.newPath)).toEqual(["main.go", "web/src/x.tsx", "logo.png"]);
    const go = files[0];
    expect(go.lang).toBe("go");
    expect(go.hunks).toHaveLength(1);
    expect(go.hunks[0].startsWith("--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@")).toBe(true);
    expect(go.hunks[0]).toContain('+import "fmt"');
    expect(go.hunks[0].split("\n")).toHaveLength(9); // file header 2 + @@ + 5 body + trailing
    const tsx = files[1];
    expect(tsx.lang).toBe("tsx");
    expect(tsx.oldPath).toBe("/dev/null");
    expect(tsx.hunks[0].startsWith("--- /dev/null\n+++ b/web/src/x.tsx\n@@")).toBe(true);
    expect(tsx.hunks[0].split("\n")).toHaveLength(6);
    expect(files[2].binary).toBe(true);
    expect(files[2].hunks).toHaveLength(0);
  });

  it("feeds a renderable DiffFile — the component's own parse path", () => {
    for (const f of toDiffViewFiles(sample).filter((x) => !x.binary)) {
      const df = new DiffFile(f.oldPath, "", f.newPath, "", f.hunks, f.lang, f.lang);
      df.initRaw();
      df.buildSplitDiffLines();
      df.buildUnifiedDiffLines();
      expect(df.fileLineLength, `${f.newPath} parsed to lines`).toBeGreaterThan(0);
      expect(df.unifiedLineLength, `${f.newPath} built unified rows`).toBeGreaterThan(0);
    }
  });

  it("counts the per-file diffstat from the hunk body sigils", () => {
    const files = toDiffViewFiles(sample);
    expect(files[0].add).toBe(1); // +import "fmt"
    expect(files[0].del).toBe(0);
    expect(files[1].add).toBe(2); // the two new-file exports
    expect(files[1].del).toBe(0);
    expect(files[2].add).toBe(0); // binary carries no counts
    expect(files[2].del).toBe(0);
  });

  it("keeps multi-hunk files as separate blocks", () => {
    const two = `diff --git a/f b/f
--- a/f
+++ b/f
@@ -1,2 +1,2 @@
 a
-b
+c
@@ -10,2 +10,2 @@
  x
-y
+z
`;
    const files = toDiffViewFiles(two);
    expect(files).toHaveLength(1);
    expect(files[0].hunks).toHaveLength(2);
    expect(files[0].hunks[1].startsWith("--- a/f\n+++ b/f\n@@ -10,2 +10,2 @@")).toBe(true);
    expect(files[0].add).toBe(2);
    expect(files[0].del).toBe(2);
  });
});

describe("langOf", () => {
  it("maps extensions and the special filenames", () => {
    expect(langOf("a/b/main.go")).toBe("go");
    expect(langOf("x.YAML")).toBe("yaml");
    expect(langOf("Dockerfile")).toBe("dockerfile");
    expect(langOf("Makefile")).toBe("makefile");
    expect(langOf("noext")).toBe("plaintext");
    expect(langOf("weird.zzz")).toBe("plaintext");
  });
});
