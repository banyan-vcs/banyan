// MergeRequestsPage.tsx — the repo's merge-request list at
// /r/:repo/-/merge_requests (the /-/ reserved separator — shareable URLs,
// IIDs never reused). Open/merged/closed filtering via the state tabs; the
// new-request form picks from the advertised branches.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { branchesOf, listRefs } from "../api/gitrefs";
import { listMRs, openMR } from "../api/mrs";
import { ErrorBox, Pill, Spinner, shortOid, whenStr } from "../components/ui";
import { useRepoBase, useRepoId } from "../repo";

const STATES = ["open", "merged", "closed"] as const;

export function MergeRequestsPage(): ReactNode {
  const repo = useRepoId();
  const base = useRepoBase();
  const [params, setParams] = useSearchParams();
  const state = (params.get("state") || "open") as (typeof STATES)[number];
  const qc = useQueryClient();
  const [source, setSource] = useState("");
  const [target, setTarget] = useState("");
  const [title, setTitle] = useState("");
  const [error, setError] = useState("");

  const mrs = useQuery({ queryKey: ["mrs", repo, state], queryFn: () => listMRs(repo, state), retry: false });
  const refs = useQuery({ queryKey: ["refs", repo], queryFn: () => listRefs(repo), retry: false });
  const branches = branchesOf(refs.data || { refs: [], capabilities: "", defaultBranch: "" });

  const create = useMutation({
    mutationFn: () => openMR(repo, { source, target, title }),
    onSuccess: (mr) => {
      setTitle("");
      setError("");
      qc.invalidateQueries({ queryKey: ["mrs", repo] });
      window.location.hash = ""; // stay; the list refreshes and the new row links on
      void mr;
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  return (
    <>
      <h1>
        merge requests · <code>{repo}</code>
      </h1>
      <div className="tab-row">
        {STATES.map((s) => (
          <button
            key={s}
            className={"tab" + (s === state ? " active" : "")}
            onClick={() => setParams(s === "open" ? {} : { state: s })}
          >
            {s}
          </button>
        ))}
      </div>

      <details className="mr-new">
        <summary>new merge request</summary>
        <form
          className="create-branch"
          onSubmit={(e) => {
            e.preventDefault();
            setError("");
            if (!source || !target || !title.trim()) {
              setError("source, target and title are required");
              return;
            }
            create.mutate();
          }}
        >
          <select value={source} onChange={(e) => setSource(e.target.value)}>
            <option value="">source branch…</option>
            {branches.map((b) => (
              <option key={b.name} value={b.name}>
                {b.name}
              </option>
            ))}
          </select>
          <span className="muted">→</span>
          <select value={target} onChange={(e) => setTarget(e.target.value)}>
            <option value="">target branch…</option>
            {branches.map((b) => (
              <option key={b.name} value={b.name}>
                {b.name}
              </option>
            ))}
          </select>
          <input placeholder="title" value={title} onChange={(e) => setTitle(e.target.value)} />
          <button className="btn" type="submit" disabled={create.isPending}>
            open
          </button>
        </form>
      </details>
      {error && <ErrorBox error={error} />}

      {mrs.isPending && <Spinner />}
      {mrs.isError && <ErrorBox error={mrs.error} />}
      {mrs.isSuccess && (mrs.data || []).length === 0 && (
        <p className="muted pad">no {state} merge requests.</p>
      )}
      {(mrs.data || []).length > 0 && (
        <table>
          <thead>
            <tr>
              <th>!</th>
              <th>title</th>
              <th>branches</th>
              <th>author</th>
              <th>updated</th>
            </tr>
          </thead>
          <tbody>
            {(mrs.data || []).map((mr) => (
              <tr key={mr.iid}>
                <td className="mono">!{mr.iid}</td>
                <td>
                  <Link to={`${base}/-/merge_requests/${mr.iid}`}>{mr.title}</Link>{" "}
                  {mr.state !== "open" && <Pill>{mr.state}</Pill>}
                </td>
                <td className="mono muted">
                  {mr.source} → {mr.target}
                </td>
                <td>{mr.author}</td>
                <td className="muted" title={String(mr.updated_at)}>
                  {whenStr(mr.updated_at)}
                  {mr.merge_commit ? ` · ${shortOid(mr.merge_commit)}` : ""}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <p className="muted pad">
        URLs are /r/{repo}/-/merge_requests/&lt;iid&gt; — IIDs never restart, merged and closed requests keep their links.
      </p>
    </>
  );
}
