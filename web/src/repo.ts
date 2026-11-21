// repo.ts — the console's two-level repository addressing: a grouped repo
// lives at /r/<group>/<project> (GitHub-shaped; the project segment IS the
// physical id the API speaks), a repo without a group keeps its legacy
// one-level /r/<name>. Pages read the physical id through useRepoId and
// build every link through useRepoBase / repoHref — never by hand.
import { useQuery } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { api } from "./api/client";
import type { GroupsResponse } from "./api/groups";
import { can, useMode } from "./mode";

/** repoHref answers a repo's console URL — group-prefixed when labeled. */
export function repoHref(repo: string, group?: string): string {
  const r = encodeURIComponent(repo);
  return group ? `/r/${encodeURIComponent(group)}/${r}` : `/r/${r}`;
}

/** useRepoId answers the physical repository id from the route: the
 * project segment on two-level routes, the repo segment on legacy ones. */
export function useRepoId(): string {
  const p = useParams();
  return p.project || p.repo || "";
}

/** useRepoGroups answers the repo → group assignment table (empty when
 * the deployment carries no groups). */
export function useRepoGroups(): Record<string, string> {
  const mode = useMode();
  const q = useQuery({
    queryKey: ["groups"],
    queryFn: () => api<GroupsResponse>("/api/v1/groups"),
    retry: false,
    enabled: can(mode, "groups"),
    staleTime: 30_000,
  });
  return q.data?.repoGroups ?? {};
}

/** useRepoBase answers the current repo page's URL prefix —
 * /r/<group>/<repo> or the legacy /r/<repo>. */
export function useRepoBase(): string {
  const repo = useRepoId();
  const groups = useRepoGroups();
  return repoHref(repo, groups[repo]);
}
