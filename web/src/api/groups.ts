// groups.ts — the groups face (docs/design/data-model.md §15): the group
// list with the repo→group assignment table, and the pure partition that
// turns them plus the flat repo registry into the repository tree's shape
// (group folders first, ungrouped roots after).
import type { RepoInfo } from "./types";

export interface GroupInfo {
  name: string;
  description: string;
  createdBy: string;
  createdAt: number;
}

export interface GroupsResponse {
  groups: GroupInfo[];
  repoGroups: Record<string, string>;
}

export interface GroupedRepos {
  /** name-sorted; each carries the repos assigned to it */
  groups: { info: GroupInfo; repos: RepoInfo[] }[];
  /** repos with no label, or a label naming a deleted group (defensive —
   * the server clears assignments on delete, stale caches may not know) */
  rest: RepoInfo[];
}

/** partitionByGroup splits the registry by the assignment table. Pure and
 * total: unknown repos in the table are ignored, unknown groups in the
 * assignments fall to `rest`, order inside a bucket is the input's. */
export function partitionByGroup(repos: RepoInfo[], assignments: Record<string, string>, groups: GroupInfo[]): GroupedRepos {
  const byGroup = new Map<string, RepoInfo[]>();
  for (const r of repos) {
    const g = assignments[r.id];
    if (g && groups.some((info) => info.name === g)) {
      const list = byGroup.get(g) ?? [];
      list.push(r);
      byGroup.set(g, list);
    }
  }
  return {
    groups: [...groups]
      .sort((a, b) => a.name.localeCompare(b.name))
      .map((info) => ({ info, repos: byGroup.get(info.name) ?? [] })),
    rest: repos.filter((r) => {
      const g = assignments[r.id];
      return !g || !groups.some((info) => info.name === g);
    }),
  };
}
