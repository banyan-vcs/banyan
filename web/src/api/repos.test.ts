// repos.test.ts — the hierarchy helpers: nesting order, depth, the stale-
// parent defense, breadcrumbs, and the cycle brake.
import { describe, expect, it } from "vitest";
import { buildRepoTree, flattenTree, repoPath } from "./repos";
import type { RepoInfo } from "./types";

const R = (id: string, parent?: string): RepoInfo => ({ id, defaultBranch: "main", parent });

describe("buildRepoTree", () => {
  it("nests children under parents, multi-level, children sorted by id", () => {
    const tree = buildRepoTree([R("monorepo"), R("zeta", "monorepo"), R("alpha", "monorepo"), R("lib", "alpha")]);
    expect(tree.map((n) => n.repo.id)).toEqual(["monorepo"]);
    const [mono] = tree;
    expect(mono.children.map((n) => n.repo.id)).toEqual(["alpha", "zeta"]);
    expect(mono.children[0].children.map((n) => n.repo.id)).toEqual(["lib"]);
    expect(mono.children[0].children[0].depth).toBe(2);
  });

  it("treats a repo with a missing parent as a root (stale cache defense)", () => {
    const tree = buildRepoTree([R("a"), R("b", "vanished")]);
    expect(tree.map((n) => n.repo.id).sort()).toEqual(["a", "b"]);
  });

  it("never nests a repo under itself", () => {
    const tree = buildRepoTree([R("loop", "loop")]);
    expect(tree).toHaveLength(1);
    expect(tree[0].children).toHaveLength(0);
  });
});

describe("flattenTree", () => {
  it("walks depth-first with depth preserved", () => {
    const flat = flattenTree(buildRepoTree([R("root"), R("kid", "root"), R("grand", "kid"), R("solo")]));
    expect(flat.map((n) => [n.repo.id, n.depth])).toEqual([
      ["root", 0],
      ["kid", 1],
      ["grand", 2],
      ["solo", 0],
    ]);
  });
});

describe("repoPath", () => {
  it("answers the ancestor chain root → repo", () => {
    const repos = [R("root"), R("kid", "root"), R("grand", "kid")];
    expect(repoPath(repos, "grand").map((r) => r.id)).toEqual(["root", "kid", "grand"]);
    expect(repoPath(repos, "root").map((r) => r.id)).toEqual(["root"]);
    expect(repoPath(repos, "nobody")).toEqual([]);
  });

  it("stops on a cycle instead of looping", () => {
    const repos = [R("a", "b"), R("b", "a")];
    expect(repoPath(repos, "a").map((r) => r.id)).toEqual(["b", "a"]);
  });
});
