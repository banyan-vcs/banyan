// groups.test.ts — the partition that shapes the repository tree: group
// folders first (name-sorted), ungrouped repos after; a label naming a
// deleted group falls back to ungrouped (the server clears assignments on
// delete, a stale cache may not know yet).
import { describe, expect, it } from "vitest";
import { partitionByGroup, type GroupInfo } from "./groups";
import type { RepoInfo } from "./types";

const g = (name: string): GroupInfo => ({ name, description: "", createdBy: "", createdAt: 0 });
const r = (id: string): RepoInfo => ({ id, defaultBranch: "main" });

describe("partitionByGroup", () => {
  it("puts labeled repos under their group, name-sorted, rest after", () => {
    const out = partitionByGroup(
      [r("demo"), r("svc"), r("site")],
      { demo: "eng", svc: "eng", site: "docs" },
      [g("eng"), g("docs")],
    );
    expect(out.groups.map((x) => x.info.name)).toEqual(["docs", "eng"]);
    expect(out.groups[0].repos.map((x) => x.id)).toEqual(["site"]);
    expect(out.groups[1].repos.map((x) => x.id)).toEqual(["demo", "svc"]);
    expect(out.rest).toEqual([]);
  });

  it("keeps input order inside a bucket", () => {
    const out = partitionByGroup([r("b"), r("a")], { b: "g", a: "g" }, [g("g")]);
    expect(out.groups[0].repos.map((x) => x.id)).toEqual(["b", "a"]);
  });

  it("drops a label naming a missing group to the rest (defensive)", () => {
    const out = partitionByGroup([r("x")], { x: "deleted" }, [g("eng")]);
    expect(out.rest.map((y) => y.id)).toEqual(["x"]);
    expect(out.groups[0].repos).toEqual([]);
  });

  it("ignores assignments for repos that are not in the registry", () => {
    const out = partitionByGroup([r("x")], { ghost: "eng" }, [g("eng")]);
    expect(out.groups[0].repos).toEqual([]);
    expect(out.rest.map((y) => y.id)).toEqual(["x"]);
  });

  it("survives the empty world", () => {
    const out = partitionByGroup([], {}, []);
    expect(out.groups).toEqual([]);
    expect(out.rest).toEqual([]);
  });
});
