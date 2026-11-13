import { describe, expect, it } from "vitest";
import { diffStat, parseUnifiedDiff } from "./diff";

const sample = `commit abc
diff --git a/main.go b/main.go
index 111..222 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
-func old() {}
+func new() {}
+func added() {}
diff --git a/README b/README
new file mode 100644
--- /dev/null
+++ b/README
@@ -0,0 +1 @@
+hello
`;

describe("parseUnifiedDiff", () => {
  const files = parseUnifiedDiff(sample);

  it("splits per file and tracks paths", () => {
    expect(files).toHaveLength(2);
    expect(files[0].newPath).toBe("main.go");
    expect(files[1].newPath).toBe("README");
  });

  it("counts rows with add/del kinds", () => {
    expect(diffStat([files[0]])).toEqual({ add: 2, del: 1 });
    expect(diffStat([files[1]])).toEqual({ add: 1, del: 0 });
  });

  it("renumbers hunk lines", () => {
    const rows = files[0].rows.filter((r) => r.kind !== "meta");
    const first = rows.find((r) => r.kind === "del");
    const added = rows.filter((r) => r.kind === "add");
    expect(first?.oldLine).toBe(2);
    expect(added[0]?.newLine).toBe(2);
    expect(added[1]?.newLine).toBe(3);
  });

  it("returns empty for no changes", () => {
    expect(parseUnifiedDiff("")).toEqual([]);
  });
});
