// MergeRequestPage.tsx — one merge request at /r/:repo/-/merge_requests/:iid.
// Shareable by construction: the IID never restarts, an unknown iid answers
// an inline not-found (never a bounce home), and the sub-views ride query
// params — ?tab=overview|diff and ?view=split|unified (view also persists
// to localStorage; the URL wins when present) — so a pasted link opens the
// same view it was shared from. The diff itself is merge-base → source-tip.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { lazy, Suspense, type ReactNode } from "react";
import { apiText } from "../api/client";
import { toDiffViewFiles } from "../api/diffview";
import { approveMR, closeMR, getMR, mergeMR, reopenMR, unapproveMR } from "../api/mrs";
// The diff view (highlight.js grammars included) is the heaviest leaf in
// the bundle — it code-splits behind the changes tab.
const MrDiff = lazy(() => import("../components/MrDiff").then((m) => ({ default: m.MrDiff })));
import { ErrorBox, Pill, Spinner, shortOid, whenStr } from "../components/ui";
import { useSession } from "../session";

const VIEW_KEY = "banyan.mr.view";

export function MergeRequestPage(): ReactNode {
  const { repo = "", iid = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "diff" ? "diff" : "overview";
  const urlView = params.get("view");
  const view: "split" | "unified" = urlView === "split" || urlView === "unified"
    ? urlView
    : localStorage.getItem(VIEW_KEY) === "unified" ? "unified" : "split";
  const setView = (v: "split" | "unified"): void => {
    localStorage.setItem(VIEW_KEY, v);
    setParams((p) => {
      const next = new URLSearchParams(p);
      next.set("view", v);
      return next;
    });
  };
  const qc = useQueryClient();
  const { info } = useSession();
  const me = info?.user || "";

  const mr = useQuery({ queryKey: ["mr", repo, iid], queryFn: () => getMR(repo, iid), retry: false });
  const view0 = mr.data;

  const diff = useQuery({
    queryKey: ["mr-diff", repo, iid, view0?.merge_base, view0?.source_tip],
    enabled: tab === "diff" && !!view0 && view0.state === "open" && !!view0.merge_base,
    queryFn: (): Promise<string> =>
      apiText(
        `/api/v1/diff?repo=${encodeURIComponent(repo)}&from=${view0?.merge_base}&to=${view0?.source_tip}`,
      ),
    retry: false,
  });

  const invalidate = (): void => {
    qc.invalidateQueries({ queryKey: ["mr", repo, iid] });
    qc.invalidateQueries({ queryKey: ["mrs", repo] });
  };
  const act = useMutation({
    mutationFn: async (what: "approve" | "unapprove" | "merge" | "close" | "reopen") => {
      const msg = what === "merge" && view0?.state === "open" ? window.prompt("merge commit message (empty = default)") ?? null : undefined;
      if (what === "merge" && msg === null) return; // cancelled
      if (what === "approve") await approveMR(repo, iid);
      if (what === "unapprove") await unapproveMR(repo, iid);
      if (what === "merge") await mergeMR(repo, iid, msg || "");
      if (what === "close") await closeMR(repo, iid);
      if (what === "reopen") await reopenMR(repo, iid);
    },
    onSuccess: invalidate,
  });
  const err = act.error ? (act.error instanceof Error ? act.error.message : String(act.error)) : "";

  if (mr.isPending) return <Spinner />;
  if (mr.isError) {
    const msg = mr.error instanceof Error ? mr.error.message : String(mr.error);
    return (
      <>
        <h1>
          !{iid} · <code>{repo}</code>
        </h1>
        {/not found|404/i.test(msg) ? (
          <p className="muted pad">
            no merge request !{iid} in {repo} — IIDs are never reused, so this link points at nothing.
          </p>
        ) : (
          <ErrorBox error={mr.error} />
        )}
      </>
    );
  }

  const open = view0?.state === "open";
  const approvedByMe = !!view0?.approvals?.includes(me);
  const approvalsNeeded =
    view0?.rule_level === "mr-only" ? Math.max(view0.required_approvals, 1) : 0;
  const approvalsMet = !approvalsNeeded || (view0?.approvals?.length ?? 0) >= approvalsNeeded;

  return (
    <>
      <h1>
        !{view0?.iid} {view0?.title} {view0 && <Pill tone={open ? "warn" : "normal"}>{view0.state}</Pill>}
      </h1>
      <p className="muted">
        <code>{repo}</code> · <span className="mono">{view0?.source} → {view0?.target}</span> · opened by{" "}
        {view0?.author} {whenStr(view0?.created_at)}
        {view0?.merge_commit && (
          <>
            {" "}· merged as{" "}
            <Link className="mono" to={`/r/${repo}/commit/${view0.merge_commit}`}>
              {shortOid(view0.merge_commit)}
            </Link>{" "}
            by {view0.merged_by}
          </>
        )}
      </p>
      {view0?.description && <p className="mr-desc">{view0.description}</p>}
      {err && <ErrorBox error={err} />}

      <div className="tab-row">
        <button className={"tab" + (tab === "overview" ? " active" : "")} onClick={() => setParams((p) => { const n = new URLSearchParams(p); n.delete("tab"); return n; })}>
          overview
        </button>
        <button className={"tab" + (tab === "diff" ? " active" : "")} onClick={() => setParams((p) => { const n = new URLSearchParams(p); n.set("tab", "diff"); return n; })}>
          changes
        </button>
      </div>

      {tab === "diff" &&
        (view0?.state !== "open" ? (
          <p className="muted pad">the request is {view0?.state} — the change view is the stored record above.</p>
        ) : !view0?.merge_base ? (
          <p className="muted pad">unrelated histories — no merge base to diff against.</p>
        ) : (
          <>
            <div className="tab-row">
              <button className={"tab" + (view === "split" ? " active" : "")} onClick={() => setView("split")}>split</button>
              <button className={"tab" + (view === "unified" ? " active" : "")} onClick={() => setView("unified")}>unified</button>
            </div>
            {diff.isPending && <Spinner />}
            {diff.isError && <ErrorBox error={diff.error} />}
            {diff.data && (
              <Suspense fallback={<Spinner />}>
                <MrDiff files={toDiffViewFiles(diff.data)} view={view} />
              </Suspense>
            )}
          </>
        ))}

      {tab === "overview" && (
        <div className="mr-overview">
          <section>
            <h3>merge status</h3>
            {open ? (
              view0?.mergeable ? (
                <p className="ok-text">mergeable — no conflicts against {view0.target}.</p>
              ) : (
                <div className="error-box">
                  conflicts: {(view0?.conflicts || []).join(", ") || "the branches conflict"} — resolve on the source
                  side, the check is live.
                </div>
              )
            ) : (
              <p className="muted">{view0?.state}.</p>
            )}
            {open && view0?.rule_level && (
              <p className="muted">
                target rule: <code>{view0.rule_level}</code>
                {approvalsNeeded > 0 && <> — needs {approvalsNeeded} approval(s) from others</>}
              </p>
            )}
            {open && (
              <p className="act-row">
                <button className="btn primary" disabled={!view0?.mergeable || !approvalsMet || act.isPending} onClick={() => act.mutate("merge")}>
                  merge
                </button>
                {me && me !== view0?.author && (
                  approvedByMe ? (
                    <button className="btn ghost" disabled={act.isPending} onClick={() => act.mutate("unapprove")}>
                      revoke my approval
                    </button>
                  ) : (
                    <button className="btn" disabled={act.isPending} onClick={() => act.mutate("approve")}>
                      approve
                    </button>
                  )
                )}
                <button className="btn ghost" disabled={act.isPending} onClick={() => act.mutate("close")}>
                  close
                </button>
              </p>
            )}
            {view0?.state === "closed" && (
              <p className="act-row">
                <button className="btn" disabled={act.isPending} onClick={() => act.mutate("reopen")}>
                  reopen
                </button>
              </p>
            )}
          </section>
          <section>
            <h3>approvals</h3>
            {(view0?.approvals || []).length === 0 ? (
              <p className="muted">none yet{approvalsNeeded > 0 ? ` — ${approvalsNeeded} needed` : ""}.</p>
            ) : (
              <ul className="approval-list">
                {(view0?.approvals || []).map((u) => (
                  <li key={u}>{u}</li>
                ))}
              </ul>
            )}
            {approvalsNeeded > 0 && (
              <p className="muted">
                {(view0?.approvals || []).length}/{approvalsNeeded} — the author's own approval never counts.
              </p>
            )}
          </section>
        </div>
      )}
    </>
  );
}
