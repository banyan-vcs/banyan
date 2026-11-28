// MergeRequestsGlobalPage.tsx — the workspace-wide merge-request list
// (GET /api/v1/merge-requests, the sidebar's cross-repo view): ledger
// rows across every visible repository, state-filtered, each row linking
// into the per-repo page where mergeability and approvals compute live.
import { useQuery } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import type { ReactNode } from "react";
import { listAllMergeRequests } from "../api/mergerequests";
import { repoHref, useRepoGroups } from "../repo";
import { useT } from "../i18n";
import { Empty, ErrorBox, Pill, Spinner, whenStr } from "../components/ui";

const STATES = ["open", "merged", "closed", "all"];

export function MergeRequestsGlobalPage(): ReactNode {
  const t = useT();
  const groups = useRepoGroups();
  const [params, setParams] = useSearchParams();
  const state = STATES.includes(params.get("state") || "") ? params.get("state")! : "open";

  const mergeRequests = useQuery({
    queryKey: ["merge-requests-all", state],
    queryFn: () => listAllMergeRequests(state === "all" ? "" : state),
  });

  const setState = (s: string): void => {
    const n = new URLSearchParams(params);
    n.set("state", s);
    setParams(n, { replace: true });
  };

  return (
    <>
      <h1>{t("Merge Requests")}</h1>
      <div className="tab-row">
        {STATES.map((s) => (
          <button key={s} className={"tab" + (state === s ? " active" : "")} onClick={() => setState(s)}>
            {t(s)}
          </button>
        ))}
      </div>
      {mergeRequests.isPending && <Spinner />}
      {mergeRequests.isError && <ErrorBox error={mergeRequests.error} />}
      {mergeRequests.isSuccess && (mergeRequests.data || []).length === 0 && <Empty>{t("nothing here — requests opened from a repository's page land in this list too.")}</Empty>}
      {mergeRequests.isSuccess && (mergeRequests.data || []).length > 0 && (
        <table>
          <thead>
            <tr>
              <th>{t("request")}</th>
              <th>{t("repository")}</th>
              <th>{t("branches")}</th>
              <th>{t("state")}</th>
              <th>{t("author")}</th>
              <th>{t("updated")}</th>
            </tr>
          </thead>
          <tbody>
            {(mergeRequests.data || []).map((mergeRequest) => (
              <tr key={`${mergeRequest.repo}#${mergeRequest.iid}`}>
                <td>
                  <Link to={repoHref(mergeRequest.repo, groups[mergeRequest.repo]) + `/-/merge_requests/${mergeRequest.iid}`}>
                    <strong>!{mergeRequest.iid}</strong> {mergeRequest.title}
                  </Link>
                </td>
                <td>
                  <Link to={repoHref(mergeRequest.repo, groups[mergeRequest.repo])} className="rowlink">
                    {mergeRequest.repo}
                  </Link>
                </td>
                <td className="muted mono">
                  {mergeRequest.source} → {mergeRequest.target}
                </td>
                <td>
                  {mergeRequest.state === "open" && <Pill tone="warn">{t("open")}</Pill>}
                  {mergeRequest.state === "merged" && <Pill tone="ok">{t("merged")}</Pill>}
                  {mergeRequest.state === "closed" && <span className="muted">{t("closed")}</span>}
                </td>
                <td>{mergeRequest.author}</td>
                <td className="muted">{whenStr(mergeRequest.updated_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
