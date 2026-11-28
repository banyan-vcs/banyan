// MergeRequestsPage.tsx — the repo's merge-request list at
// /r/:repo/-/merge_requests (the /-/ reserved separator — shareable URLs,
// IIDs never reused). Open/merged/closed filtering via the state tabs; the
// new-request form picks from the advertised branches.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { branchesOf, listRefs } from "../api/gitrefs";
import { listMergeRequests, openMergeRequest } from "../api/mergerequests";
import { ErrorBox, Pill, Spinner, shortOid, whenStr } from "../components/ui";
import { useRepoBase, useRepoId } from "../repo";
import { useT } from "../i18n";

const STATES = ["open", "merged", "closed"] as const;

export function MergeRequestsPage(): ReactNode {
  const t = useT();
  const repo = useRepoId();
  const base = useRepoBase();
  const [params, setParams] = useSearchParams();
  const state = (params.get("state") || "open") as (typeof STATES)[number];
  const qc = useQueryClient();
  const [source, setSource] = useState("");
  const [target, setTarget] = useState("");
  const [title, setTitle] = useState("");
  const [error, setError] = useState("");

  const mergeRequests = useQuery({ queryKey: ["merge-requests", repo, state], queryFn: () => listMergeRequests(repo, state), retry: false });
  const refs = useQuery({ queryKey: ["refs", repo], queryFn: () => listRefs(repo), retry: false });
  const branches = branchesOf(refs.data || { refs: [], capabilities: "", defaultBranch: "" });

  const create = useMutation({
    mutationFn: () => openMergeRequest(repo, { source, target, title }),
    onSuccess: (mergeRequest) => {
      setTitle("");
      setError("");
      qc.invalidateQueries({ queryKey: ["merge-requests", repo] });
      window.location.hash = ""; // stay; the list refreshes and the new row links on
      void mergeRequest;
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  return (
    <>
      <h1>
        {t("Merge Requests")} · <code>{repo}</code>
      </h1>
      <div className="tab-row">
        {STATES.map((s) => (
          <button
            key={s}
            className={"tab" + (s === state ? " active" : "")}
            onClick={() => setParams(s === "open" ? {} : { state: s })}
          >
            {t(s)}
          </button>
        ))}
      </div>

      <details className="merge-request-new">
        <summary>{t("new merge request")}</summary>
        <form
          className="create-branch"
          onSubmit={(e) => {
            e.preventDefault();
            setError("");
            if (!source || !target || !title.trim()) {
              setError(t("source, target and title are required"));
              return;
            }
            create.mutate();
          }}
        >
          <select value={source} onChange={(e) => setSource(e.target.value)}>
            <option value="">{t("source branch…")}</option>
            {branches.map((b) => (
              <option key={b.name} value={b.name}>
                {b.name}
              </option>
            ))}
          </select>
          <span className="muted">→</span>
          <select value={target} onChange={(e) => setTarget(e.target.value)}>
            <option value="">{t("target branch…")}</option>
            {branches.map((b) => (
              <option key={b.name} value={b.name}>
                {b.name}
              </option>
            ))}
          </select>
          <input placeholder={t("title")} value={title} onChange={(e) => setTitle(e.target.value)} />
          <button className="btn" type="submit" disabled={create.isPending}>
            {t("open request")}
          </button>
        </form>
      </details>
      {error && <ErrorBox error={error} />}

      {mergeRequests.isPending && <Spinner />}
      {mergeRequests.isError && <ErrorBox error={mergeRequests.error} />}
      {mergeRequests.isSuccess && (mergeRequests.data || []).length === 0 && (
        <p className="muted pad">{t("no {0} merge requests.", t(state))}</p>
      )}
      {(mergeRequests.data || []).length > 0 && (
        <table>
          <thead>
            <tr>
              <th>!</th>
              <th>{t("title")}</th>
              <th>{t("branches")}</th>
              <th>{t("author")}</th>
              <th>{t("updated")}</th>
            </tr>
          </thead>
          <tbody>
            {(mergeRequests.data || []).map((mergeRequest) => (
              <tr key={mergeRequest.iid}>
                <td className="mono">!{mergeRequest.iid}</td>
                <td>
                  <Link to={`${base}/-/merge_requests/${mergeRequest.iid}`}>{mergeRequest.title}</Link>{" "}
                  {mergeRequest.state !== "open" && <Pill>{t(mergeRequest.state)}</Pill>}
                </td>
                <td className="mono muted">
                  {mergeRequest.source} → {mergeRequest.target}
                </td>
                <td>{mergeRequest.author}</td>
                <td className="muted" title={String(mergeRequest.updated_at)}>
                  {whenStr(mergeRequest.updated_at)}
                  {mergeRequest.merge_commit ? ` · ${shortOid(mergeRequest.merge_commit)}` : ""}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <p className="muted pad">
        {t(
          "URLs are /r/{0}/-/merge_requests/<iid> — IIDs never restart, merged and closed requests keep their links.",
          repo
        )}
      </p>
    </>
  );
}
