// ReposPage.tsx — the repository list plus the create form. In GitLab SSO
// mode repo ids are sha256(project_id); the BFF maps them back to project
// paths for display (only what the user can see).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import type { RepoInfo } from "../api/types";
import { can, useMode } from "../mode";
import { Empty, ErrorBox, Spinner } from "../components/ui";

export function ReposPage(): ReactNode {
  const nav = useNavigate();
  const mode = useMode();
  const qc = useQueryClient();
  const [name, setName] = useState("");
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
    mutationFn: () => api("/api/v1/repos", { method: "POST", body: JSON.stringify({ name }) }),
    onSuccess: () => {
      setName("");
      setError("");
      qc.invalidateQueries({ queryKey: ["repos"] });
      nav(`/r/${name}`);
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  const display = (r: RepoInfo): { id: string; label: string } => {
    const mapped = names.data?.names[r.id];
    return { id: r.id, label: mapped || r.id };
  };

  return (
    <>
      <h1>repositories</h1>
      {repos.isPending && <Spinner />}
      {repos.isError && <ErrorBox error={repos.error} />}
      {repos.isSuccess && (
        <>
          {repos.data.repos.length === 0 ? (
            <Empty>no repositories yet — push one (first push auto-creates) or create one below.</Empty>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>repository</th>
                  <th>default branch</th>
                </tr>
              </thead>
              <tbody>
                {repos.data.repos.map((r) => (
                  <tr key={r.id}>
                    <td>
                      <Link to={`/r/${r.id}`}>{display(r).label}</Link>
                    </td>
                    <td className="muted">{r.defaultBranch || "main"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
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
