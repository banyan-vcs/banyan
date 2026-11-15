// ReposPage.tsx — the repository list as a project tree plus the create
// form. The registry list is flat with a parent link (docs/design/data-model.md §14);
// buildRepoTree/flattenTree nest it — collapse state is local. In GitLab SSO
// mode repo ids are sha256(project_id); the BFF maps them back to project
// paths for display (only what the user can see).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import { buildRepoTree, flattenTree, repoPath } from "../api/repos";
import type { RepoInfo } from "../api/types";
import { can, useMode } from "../mode";
import { Empty, ErrorBox, Spinner } from "../components/ui";

function RepoTree({ repos, display }: { repos: RepoInfo[]; display: (r: RepoInfo) => string }): ReactNode {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const rows = flattenTree(buildRepoTree(repos)).filter((n) => {
    // A collapsed node hides its whole subtree: walk up the (implicit) tree
    // via the parent links and drop rows under any collapsed ancestor.
    let p = n.repo.parent;
    while (p) {
      if (collapsed.has(p)) return false;
      const parent = repos.find((r) => r.id === p);
      p = parent?.parent;
    }
    return true;
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
        {rows.map((n) => {
          const kids = n.children.length > 0;
          const isOpen = !collapsed.has(n.repo.id);
          return (
            <tr key={n.repo.id}>
              <td style={{ paddingLeft: `${n.depth * 1.4}em`, whiteSpace: "nowrap" }}>
                {kids ? (
                  <button
                    className="link"
                    title={isOpen ? "collapse" : "expand"}
                    onClick={() =>
                      setCollapsed((prev) => {
                        const next = new Set(prev);
                        if (next.has(n.repo.id)) next.delete(n.repo.id);
                        else next.add(n.repo.id);
                        return next;
                      })
                    }
                  >
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
        })}
      </tbody>
    </table>
  );
}

export function ReposPage(): ReactNode {
  const nav = useNavigate();
  const mode = useMode();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [parent, setParent] = useState("");
  const [error, setError] = useState("");
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
  const create = useMutation({
    mutationFn: () =>
      api("/api/v1/repos", {
        method: "POST",
        body: JSON.stringify(parent ? { name, parent } : { name }),
      }),
    onSuccess: () => {
      setName("");
      setError("");
      qc.invalidateQueries({ queryKey: ["repos"] });
      nav(`/r/${name}`);
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
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
            <Empty>no repositories yet — push one (first push auto-creates) or create one below.</Empty>
          ) : (
            <RepoTree repos={list} display={display} />
          )}
          {can(mode, "createRepos") && (
            <form
              className="create-repo"
              onSubmit={(e) => {
                e.preventDefault();
                if (name.trim()) create.mutate();
              }}
            >
              <input placeholder="new repository (single segment)" value={name} onChange={(e) => setName(e.target.value)} />
              <select value={parent} onChange={(e) => setParent(e.target.value)} title="nest under a parent project">
                <option value="">— root project —</option>
                {list.map((r) => (
                  <option key={r.id} value={r.id}>
                    {repoPath(list, r.id).map((p) => display(p)).join(" / ")}
                  </option>
                ))}
              </select>
              <button className="btn" type="submit" disabled={create.isPending}>
                create
              </button>
              {error && <span className="muted">{error}</span>}
            </form>
          )}
          {can(mode, "import") && (
            <p className="pad">
              <Link to="/settings/imports">import a repository →</Link>
            </p>
          )}
        </>
      )}
    </>
  );
}
