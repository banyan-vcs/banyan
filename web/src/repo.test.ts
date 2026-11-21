// repo.test.ts — the two-level addressing helper: a grouped repo's URL is
// /r/<group>/<project>, an ungrouped one keeps the legacy /r/<name>, and
// both segments encode (defensive — names are single-segment by rule).
import { describe, expect, it } from "vitest";
import { repoHref } from "./repo";

describe("repoHref", () => {
  it("prefixes the group when labeled", () => {
    expect(repoHref("demo", "platform")).toBe("/r/platform/demo");
  });

  it("keeps the legacy one-level shape without a group", () => {
    expect(repoHref("demo")).toBe("/r/demo");
    expect(repoHref("demo", undefined)).toBe("/r/demo");
  });

  it("encodes both segments", () => {
    expect(repoHref("a b", "g h")).toBe("/r/g%20h/a%20b");
  });
});
