// ReposPage.tsx — the repository list, GitHub-shaped after the two-level
// addressing (docs/design/data-model.md §15): one flat row per repository
// — name (its group's path prefix muted in front), the description
// truncated by the table's width budget, the default branch's latest
// commit time. Group folders and the hierarchy tree are gone; the group
// IS the path now. In GitLab SSO mode repo ids are sha256(project_id);
// the BFF maps them back to project paths for display (only what the
// user can see). Creation lives at /new (the topbar "+").
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { ReactNode } from "react";
import { api } from "../api/client";
import type { GroupInfo, GroupsResponse } from "../api/groups";
import type { RepoInfo } from "../api/types";
import { repoHref } from "../repo";
import { can, useMode } from "../mode";
import { useT } from "../i18n";
import { Empty, ErrorBox, Spinner, whenStr } from "../components/ui";

export function ReposPage(): ReactNode {
  const t = useT();
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
  const assignments = groups.data?.repoGroups ?? {};
  const groupOf = (id: string): GroupInfo | undefined =>
    (groups.data?.groups ?? []).find((g) => g.name === assignments[id]);
  const list = [...(repos.data?.repos ?? [])].sort((a, b) => (b.updatedAt ?? 0) - (a.updatedAt ?? 0));

  return (
    <>
      <h1>{t("Repositories")}</h1>
      {repos.isPending && <Spinner />}
      {repos.isError && <ErrorBox error={repos.error} />}
      {repos.isSuccess && (
        <>
          {list.length === 0 ? (
            <Empty>
              {t(
                "no repositories yet — push one (first push auto-creates) or use the {0} in the top bar.",
                <Link to="/new?kind=repo">+</Link>
              )}
            </Empty>
          ) : (
            <table className="repo-list">
              <thead>
                <tr>
                  <th>{t("repository")}</th>
                  <th>{t("description")}</th>
                  <th>{t("updated")}</th>
                </tr>
              </thead>
              <tbody>
                {list.map((r) => {
                  const g = groupOf(r.id);
                  return (
                    <tr key={r.id}>
                      <td>
                        <Link to={repoHref(r.id, g?.name)} className="repo-name">
                          {g && <span className="muted">{g.name} / </span>}
                          {display(r)}
                        </Link>
                      </td>
                      <td className="cell-desc" title={r.description}>
                        {r.description || <span className="muted">—</span>}
                      </td>
                      <td className="muted">{r.updatedAt ? whenStr(r.updatedAt) : "—"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </>
      )}
    </>
  );
}
