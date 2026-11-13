// BranchesPage.tsx — branch management: the list (your filtered view) plus
// create/delete through the same ref CAS a git push rides (POST /api/v1/refs).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import { branchesOf, listRefs } from "../api/gitrefs";
import { RepoTabs } from "../components/RepoTabs";
import { ErrorBox, Pill, Spinner, shortOid } from "../components/ui";

export function BranchesPage(): ReactNode {
  const { repo = "" } = useParams();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [from, setFrom] = useState("");
  const [error, setError] = useState("");
  const refs = useQuery({ queryKey: ["refs", repo], queryFn: () => listRefs(repo), retry: false });
  const branches = branchesOf(refs.data || { refs: [], capabilities: "", defaultBranch: "" });

  const invalidate = (): void => {
    qc.invalidateQueries({ queryKey: ["refs", repo] });
  };
  const create = useMutation({
    mutationFn: async () => {
      // the select defaults to the first row (default branch sorts first)
      const source = branches.find((b) => b.name === from) || branches[0];
      if (!source) throw new Error("no source branch to branch from");
      await api("/api/v1/refs", {
        method: "POST",
        body: JSON.stringify({ repo, name: `refs/heads/${name}`, old: "", new: source.oid }),
      });
    },
    onSuccess: () => {
      setName("");
      setError("");
      invalidate();
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  const del = useMutation({
    mutationFn: (branch: string) => {
      const b = branches.find((x) => x.name === branch);
      if (!b) throw new Error("branch vanished");
      return api("/api/v1/refs", {
        method: "POST",
        body: JSON.stringify({ repo, name: `refs/heads/${branch}`, old: b.oid, new: "" }),
      });
    },
    onSuccess: invalidate,
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  return (
    <>
      <h1>
        branches · <code>{repo}</code>
      </h1>
      <RepoTabs repo={repo} current="branches" />
      {error && <div className="error-box">{error}</div>}
      <form
        className="create-branch"
        onSubmit={(e) => {
          e.preventDefault();
          setError("");
          if (name.includes("/")) { setError("branch names are single-segment in banyan (the timeline model) — no slashes"); return; }
          if (name.trim()) create.mutate();
        }}
      >
        <input placeholder="new branch" value={name} onChange={(e) => setName(e.target.value)} />
        <select value={from || branches[0]?.name || ""} onChange={(e) => setFrom(e.target.value)}>
          {branches.map((b) => (
            <option key={b.name} value={b.name}>
              from {b.name}
            </option>
          ))}
        </select>
        <button className="btn" type="submit" disabled={create.isPending || branches.length === 0}>
          create
        </button>
      </form>
      {refs.isPending && <Spinner />}
      {refs.isError && <ErrorBox error={refs.error} />}
      {refs.isSuccess && branches.length === 0 && <p className="muted pad">no branches yet — push one.</p>}
      {branches.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>branch</th>
              <th>head</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {branches.map((b) => (
              <tr key={b.name}>
                <td>
                  <Link to={`/r/${repo}/tree?ref=${encodeURIComponent(b.name)}`}>{b.name}</Link>
                  {b.isDefault && <Pill>default</Pill>}
                </td>
                <td>
                  <Link to={`/r/${repo}/commit/${b.oid}`} className="muted mono">
                    {shortOid(b.oid)}
                  </Link>
                </td>
                <td>
                  {!b.isDefault && (
                    <button
                      className="btn ghost danger"
                      disabled={del.isPending}
                      onClick={() => {
                        if (window.confirm(`delete branch ${b.name}?`)) del.mutate(b.name);
                      }}
                    >
                      delete
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <p className="muted pad">creation and deletion go through the same ref CAS (old→new) a git push rides.</p>
    </>
  );
}
