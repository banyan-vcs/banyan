// repos.ts — the project-hierarchy helpers (docs/design/data-model.md §14): the
// registry list is flat with a parent link; the tree, the breadcrumb path
// and the flattened rendering order derive from it here, pure and testable.
import type { RepoInfo } from "./types";

export interface RepoTreeNode {
  repo: RepoInfo;
  depth: number;
  children: RepoTreeNode[];
}

// buildRepoTree nests the flat registry. Children sort by id inside each
// parent; a repo whose parent is missing from the list (deleted upstream,
// stale cache) renders as a root — defensive, never a blank row.
export function buildRepoTree(repos: RepoInfo[]): RepoTreeNode[] {
  const byId = new Map(repos.map((r) => [r.id, r]));
  const sorted = [...repos].sort((a, b) => a.id.localeCompare(b.id));
  const childrenOf = new Map<string, RepoInfo[]>();
  const roots: RepoInfo[] = [];
  for (const r of sorted) {
    if (r.parent && r.parent !== r.id && byId.has(r.parent)) {
      const list = childrenOf.get(r.parent) ?? [];
      list.push(r);
      childrenOf.set(r.parent, list);
    } else {
      roots.push(r);
    }
  }
  const build = (repo: RepoInfo, depth: number): RepoTreeNode => ({
    repo,
    depth,
    children: (childrenOf.get(repo.id) ?? []).map((c) => build(c, depth + 1)),
  });
  return roots.map((r) => build(r, 0));
}

// flattenTree walks depth-first, keeping the depth on every row — the table
// renders one row per repo with indent.
export function flattenTree(nodes: RepoTreeNode[]): RepoTreeNode[] {
  const out: RepoTreeNode[] = [];
  const walk = (list: RepoTreeNode[]): void => {
    for (const n of list) {
      out.push(n);
      walk(n.children);
    }
  };
  walk(nodes);
  return out;
}

// repoPath answers the ancestor chain root → repo (a cycle guard stops the
// walk; the server refuses links that could form one, this is belt and
// braces against stale caches).
export function repoPath(repos: RepoInfo[], id: string): RepoInfo[] {
  const byId = new Map(repos.map((r) => [r.id, r]));
  const path: RepoInfo[] = [];
  const seen = new Set<string>();
  let cur = byId.get(id);
  while (cur && !seen.has(cur.id)) {
    seen.add(cur.id);
    path.unshift(cur);
    cur = cur.parent ? byId.get(cur.parent) : undefined;
  }
  return path;
}
