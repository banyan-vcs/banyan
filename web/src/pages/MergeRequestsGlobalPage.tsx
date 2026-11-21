// MergeRequestsGlobalPage.tsx — the workspace-wide merge-request list
// (GET /api/v1/merge-requests, the sidebar's cross-repo view): ledger
// rows across every visible repository, state-filtered, each row linking
// into the per-repo page where mergeability and approvals compute live.
import { useQuery } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router-dom";
import type { ReactNode } from "react";
import { listAllMRs } from "../api/mrs";
import { repoHref, useRepoGroups } from "../repo";
import { Empty, ErrorBox, Pill, Spinner, whenStr } from "../components/ui";

const STATES = ["open", "merged", "closed", "all"];

export function MergeRequestsGlobalPage(): ReactNode {
  const groups = useRepoGroups();
  const [params, setParams] = useSearchParams();
  const state = STATES.includes(params.get("state") || "") ? params.get("state")! : "open";

  const mrs = useQuery({
    queryKey: ["mrs-all", state],
    queryFn: () => listAllMRs(state === "all" ? "" : state),
  });

  const setState = (s: string): void => {
    const n = new URLSearchParams(params);
    n.set("state", s);
    setParams(n, { replace: true });
  };

  return (
    <>
      <h1>merge requests</h1>
      <div className="tab-row">
        {STATES.map((s) => (
          <button key={s} className={"tab" + (state === s ? " active" : "")} onClick={() => setState(s)}>
            {s}
          </button>
        ))}
      </div>
      {mrs.isPending && <Spinner />}
      {mrs.isError && <ErrorBox error={mrs.error} />}
      {mrs.isSuccess && (mrs.data || []).length === 0 && <Empty>nothing here — requests opened from a repository's page land in this list too.</Empty>}
      {mrs.isSuccess && (mrs.data || []).length > 0 && (
        <table>
          <thead>
            <tr>
              <th>request</th>
              <th>repository</th>
              <th>branches</th>
              <th>state</th>
              <th>author</th>
              <th>updated</th>
            </tr>
          </thead>
          <tbody>
            {(mrs.data || []).map((mr) => (
              <tr key={`${mr.repo}#${mr.iid}`}>
                <td>
                  <Link to={repoHref(mr.repo, groups[mr.repo]) + `/-/merge_requests/${mr.iid}`}>
                    <strong>!{mr.iid}</strong> {mr.title}
                  </Link>
                </td>
                <td>
                  <Link to={repoHref(mr.repo, groups[mr.repo])} className="rowlink">
                    {mr.repo}
                  </Link>
                </td>
                <td className="muted mono">
                  {mr.source} → {mr.target}
                </td>
                <td>
                  {mr.state === "open" && <Pill tone="warn">open</Pill>}
                  {mr.state === "merged" && <Pill tone="ok">merged</Pill>}
                  {mr.state === "closed" && <span className="muted">closed</span>}
                </td>
                <td>{mr.author}</td>
                <td className="muted">{whenStr(mr.updated_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
