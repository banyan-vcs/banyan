// GroupsPage.tsx — the groups surface (docs/design/data-model.md §15):
// every group with its members (the assignment table joined against the
// registry), creation pointing at the /new page, and deletion (write
// rank — dropping a group unlabels its repos, nothing else).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { ReactNode } from "react";
import { api } from "../api/client";
import { partitionByGroup, type GroupsResponse } from "../api/groups";
import { repoHref } from "../repo";
import type { RepoInfo } from "../api/types";
import { can, useMode } from "../mode";
import { Empty, ErrorBox, Spinner, whenStr } from "../components/ui";

export function GroupsPage(): ReactNode {
  const mode = useMode();
  const qc = useQueryClient();
  const groups = useQuery({
    queryKey: ["groups"],
    queryFn: () => api<GroupsResponse>("/api/v1/groups"),
    retry: false,
  });
  const repos = useQuery({
    queryKey: ["repos"],
    queryFn: () => api<{ repos: RepoInfo[] }>("/api/v1/repos"),
  });
  const del = useMutation({
    mutationFn: (name: string) => api(`/api/v1/groups/${encodeURIComponent(name)}`, { method: "DELETE" }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["groups"] });
      qc.invalidateQueries({ queryKey: ["repos"] });
    },
  });

  if (groups.isPending) return <Spinner />;
  if (groups.isError) return <ErrorBox error={groups.error} />;

  const list = groups.data.groups ?? [];
  const assignments = groups.data.repoGroups ?? {};
  const parts = partitionByGroup(repos.data?.repos ?? [], assignments, list);

  return (
    <>
      <h1>groups</h1>
      <p className="muted">
        a group is an organizational container: it folders the repository list and gates nothing — names and URLs stay
        untouched. Repositories take the label at creation or from their settings page.
      </p>
      {can(mode, "groups") && (
        <p className="pad">
          <Link className="btn" to="/new?kind=group">
            new group
          </Link>
        </p>
      )}
      {list.length === 0 && <Empty>no groups yet.</Empty>}
      {list.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>group</th>
              <th>repositories</th>
              <th>created</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {parts.groups.map(({ info, repos: members }) => (
              <tr key={info.name}>
                <td>
                  <strong>{info.name}</strong>
                  {info.description && <span className="muted"> — {info.description}</span>}
                </td>
                <td>
                  {members.length === 0 && <span className="muted">—</span>}
                  {members.map((r) => (
                    <Link key={r.id} to={repoHref(r.id, assignments[r.id])} className="rowlink">
                      {r.id}
                    </Link>
                  ))}
                </td>
                <td className="muted">
                  {info.createdBy} · {whenStr(info.createdAt)}
                </td>
                <td>
                  <button
                    className="btn ghost danger"
                    disabled={del.isPending}
                    onClick={() => {
                      if (window.confirm(`delete group ${info.name}? its repositories keep their names — the label just goes away.`))
                        del.mutate(info.name);
                    }}
                  >
                    delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
