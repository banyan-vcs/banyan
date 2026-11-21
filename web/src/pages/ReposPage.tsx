// ReposPage.tsx — the repository list: groups as folders (each nesting the
// parent-hierarchy trees of its members), the ungrouped projects after
// (docs/design/data-model.md §14 and §15). The registry list is flat with
// a parent link; buildRepoTree/flattenTree nest it; collapse state is
// local — group folders ride the same set under "group:<name>" keys. In
// GitLab SSO mode repo ids are sha256(project_id); the BFF maps them back
// to project paths for display (only what the user can see). The create
// form itself moved to /new (the topbar "+").
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Fragment, useState, type ReactNode } from "react";
import { api } from "../api/client";
import { buildRepoTree, flattenTree } from "../api/repos";
import { partitionByGroup, type GroupInfo, type GroupsResponse } from "../api/groups";
import type { RepoInfo } from "../api/types";
import { can, useMode } from "../mode";
import { Empty, ErrorBox, Spinner } from "../components/ui";

/** RepoTree renders the whole registry as one table: group folders first
 * (caret + name + count, collapsible), their repos' hierarchy trees
 * indented one level under them, the ungrouped roots at the top level. */
function RepoTree({
  repos,
  display,
  groups,
  assignments,
}: {
  repos: RepoInfo[];
  display: (r: RepoInfo) => string;
  groups: GroupInfo[];
  assignments: Record<string, string>;
}): ReactNode {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const toggle = (key: string): void =>
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const parts = partitionByGroup(repos, assignments, groups);
  // One row per repo, honoring the parent-carets above it (the same
  // ancestor walk as before, scoped to the repo's bucket) and the group
  // folder's own collapse.
  const repoRows = (bucket: RepoInfo[], keyPrefix: string, indentOffset: number): ReactNode =>
    flattenTree(buildRepoTree(bucket))
      .filter((n) => {
        let p = n.repo.parent;
        while (p) {
          if (collapsed.has(p)) return false;
          const parent = bucket.find((r) => r.id === p);
          p = parent?.parent;
        }
        return true;
      })
      .map((n) => {
        const kids = n.children.length > 0;
        const isOpen = !collapsed.has(n.repo.id);
        return (
          <tr key={keyPrefix + n.repo.id}>
            <td style={{ paddingLeft: `${(n.depth + indentOffset) * 1.4}em`, whiteSpace: "nowrap" }}>
              {kids ? (
                <button className="link" title={isOpen ? "collapse" : "expand"} onClick={() => toggle(n.repo.id)}>
                  {isOpen ? "▾" : "▸"}
                </button>
              ) : (
                <span className="muted">·</span>
              )}{" "}
              <Link to={`/r/${n.repo.id}`}>{display(n.repo)}</Link>
              {kids && <span className="muted"> ({n.children.length})</span>}
            </td>
            <td className="muted">{n.repo.defaultBranch || "main"}</td>
          </tr>
        );
      });

  return (
    <table>
      <thead>
        <tr>
          <th>repository</th>
          <th>default branch</th>
        </tr>
      </thead>
      <tbody>
        {parts.groups.map(({ info, repos: bucket }) => {
          const key = `group:${info.name}`;
          const open = !collapsed.has(key);
          return (
            <Fragment key={key}>
              <tr className="group-row">
                <td colSpan={2}>
                  <button className="link" title={open ? "collapse" : "expand"} onClick={() => toggle(key)}>
                    {open ? "▾" : "▸"}
                  </button>{" "}
                  <strong>{info.name}</strong>
                  <span className="muted"> ({bucket.length})</span>
                  {info.description && <span className="muted"> — {info.description}</span>}
                </td>
              </tr>
              {open && repoRows(bucket, key + ":", 1)}
            </Fragment>
          );
        })}
        {repoRows(parts.rest, "", 0)}
      </tbody>
    </table>
  );
}

export function ReposPage(): ReactNode {
  const mode = useMode();
  const repos = useQuery({
    queryKey: ["repos"],
    queryFn: () => api<{ repos: RepoInfo[] }>("/api/v1/repos"),
  });
  const names = useQuery({
    queryKey: ["gitlab-names"],
    queryFn: () => api<{ names: Record<string, string> }>("/api/gitlab-names"),
    retry: false,
    staleTime: 60_000,
  });
  const groups = useQuery({
    queryKey: ["groups"],
    queryFn: () => api<GroupsResponse>("/api/v1/groups"),
    retry: false,
    enabled: can(mode, "groups"),
  });

  const display = (r: RepoInfo): string => names.data?.names[r.id] || r.id;
  const list = repos.data?.repos ?? [];

  return (
    <>
      <h1>repositories</h1>
      {repos.isPending && <Spinner />}
      {repos.isError && <ErrorBox error={repos.error} />}
      {repos.isSuccess && (
        <>
          {list.length === 0 ? (
            <Empty>
              no repositories yet — push one (first push auto-creates) or use the <Link to="/new?kind=repo">+</Link> in
              the top bar.
            </Empty>
          ) : (
            <RepoTree
              repos={list}
              display={display}
              groups={groups.data?.groups ?? []}
              assignments={groups.data?.repoGroups ?? {}}
            />
          )}
        </>
      )}
    </>
  );
}
