// commitEdit.test.ts — the blob-oid composition is load-bearing: a missing
// set() offset once made the content overwrite the header, so every web
// edit (tree and merge request alike) landed as "hash mismatch" on the server. The
// helper is pinned against git's reference values, node-computed here.
import { createHash } from "node:crypto";
import { describe, expect, it } from "vitest";
import { blobOidHex } from "./commitEdit";

const gitBlobOid = (content: string): string => {
  const bytes = Buffer.from(content, "utf8");
  return createHash("sha1").update(`blob ${bytes.length}\0`).update(bytes).digest("hex");
};

describe("blobOidHex", () => {
  it("computes git blob oids byte-exactly (incl. empty + non-ascii)", async () => {
    const cases = ["hello browser\n", "package main\n\nfunc main() {}\n", "", "ünïcode ✓\n"];
    for (const c of cases) {
      expect(await blobOidHex(c)).toBe(gitBlobOid(c));
    }
    expect(await blobOidHex("")).toBe("e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"); // git's empty blob
  });
});
