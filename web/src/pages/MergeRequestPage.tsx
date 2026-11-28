// MergeRequestPage.tsx — one merge request at /r/:repo/-/merge_requests/:iid.
// Shareable by construction: the IID never restarts, an unknown iid answers
// an inline not-found (never a bounce home), and the sub-views ride query
// params — ?tab=overview|diff and ?view=split|unified (view also persists
// to localStorage; the URL wins when present) — so a pasted link opens the
// same view it was shared from. The diff itself is merge-base → source-tip.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { lazy, Suspense, useState, type ReactNode } from "react";
import { apiText } from "../api/client";
import { DIFF_VIEW_KEY, toDiffViewFiles } from "../api/diffview";
import { addMergeRequestComment, approveMergeRequest, closeMergeRequest, getDiscussion, getMergeRequest, mergeMergeRequest, reopenMergeRequest, unapproveMergeRequest } from "../api/mergerequests";
// The diff view (highlight.js grammars included) is the heaviest leaf in
// the bundle — it code-splits behind the changes tab, as does the editor
// the inline edit opens.
const MergeRequestDiff = lazy(() => import("../components/MergeRequestDiff").then((m) => ({ default: m.MergeRequestDiff })));
const MergeRequestFileEdit = lazy(() => import("../components/MergeRequestFileEdit").then((m) => ({ default: m.MergeRequestFileEdit })));
const MergeRequestTimeline = lazy(() => import("../components/MergeRequestDiscussion").then((m) => ({ default: m.MergeRequestTimeline })));
const MergeRequestReviewBar = lazy(() => import("../components/MergeRequestDiscussion").then((m) => ({ default: m.MergeRequestReviewBar })));
import { Avatar } from "../components/Avatar";
import { ErrorBox, Pill, Spinner, shortOid, whenStr } from "../components/ui";
import { useSession } from "../session";
import { useRepoBase, useRepoId } from "../repo";
import { useT } from "../i18n";

// One split/unified memory across the diff surfaces (the commit page rides
// the same key); this page keeps the URL ?view= override for shareability.
const VIEW_KEY = DIFF_VIEW_KEY;

export function MergeRequestPage(): ReactNode {
  const t = useT();
  const { iid = "" } = useParams();
  const repo = useRepoId();
  const base = useRepoBase();
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
  // The inline edit: one file of the change view edited straight onto the
  // source branch (Monaco, code-split); the diff and mergeability recompute
  // the moment the commit lands.
  const [editingPath, setEditingPath] = useState("");

  const mergeRequest = useQuery({ queryKey: ["mergeRequest", repo, iid], queryFn: () => getMergeRequest(repo, iid), retry: false });
  const view0 = mergeRequest.data;

  // The discussion ledger + approval events — the timeline's data set.
  const discussion = useQuery({
    queryKey: ["merge-request-discussion", repo, iid],
    queryFn: () => getDiscussion(repo, iid),
    enabled: !!view0,
    retry: false,
  });
  const comment = useMutation({
    mutationFn: (a: { body: string; path?: string; side?: "old" | "new"; line?: number; parentID?: number }) =>
      addMergeRequestComment(repo, iid, {
        body: a.body,
        path: a.path,
        side: a.side,
        line: a.line,
        parent_id: a.parentID || 0,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["merge-request-discussion", repo, iid] });
      qc.invalidateQueries({ queryKey: ["mergeRequest", repo, iid] });
    },
  });

  const diff = useQuery({
    queryKey: ["diff-browser", repo, iid, view0?.merge_base, view0?.source_tip],
    enabled: tab === "diff" && !!view0 && view0.state === "open" && !!view0.merge_base,
    queryFn: (): Promise<string> =>
      apiText(
        `/api/v1/diff?repo=${encodeURIComponent(repo)}&from=${view0?.merge_base}&to=${view0?.source_tip}`,
      ),
    retry: false,
  });

  const invalidate = (): void => {
    qc.invalidateQueries({ queryKey: ["mergeRequest", repo, iid] });
    qc.invalidateQueries({ queryKey: ["merge-requests", repo] });
  };
  const act = useMutation({
    mutationFn: async (what: "approve" | "unapprove" | "merge" | "close" | "reopen") => {
      const msg = what === "merge" && view0?.state === "open" ? window.prompt(t("merge commit message (empty = default)")) ?? null : undefined;
      if (what === "merge" && msg === null) return; // cancelled
      if (what === "approve") await approveMergeRequest(repo, iid);
      if (what === "unapprove") await unapproveMergeRequest(repo, iid);
      if (what === "merge") await mergeMergeRequest(repo, iid, msg || "");
      if (what === "close") await closeMergeRequest(repo, iid);
      if (what === "reopen") await reopenMergeRequest(repo, iid);
    },
    onSuccess: invalidate,
  });
  const err = act.error ? (act.error instanceof Error ? act.error.message : String(act.error)) : "";

  if (mergeRequest.isPending) return <Spinner />;
  if (mergeRequest.isError) {
    const msg = mergeRequest.error instanceof Error ? mergeRequest.error.message : String(mergeRequest.error);
    return (
      <>
        <h1>
          !{iid} · <code>{repo}</code>
        </h1>
        {/not found|404/i.test(msg) ? (
          <p className="muted pad">{t("no merge request !{0} in {1} — IIDs are never reused, so this link points at nothing.", iid, repo)}</p>
        ) : (
          <ErrorBox error={mergeRequest.error} />
        )}
      </>
    );
  }

  const open = view0?.state === "open";
  const approvedByMe = !!view0?.approvals?.includes(me);
  const approvalsNeeded =
    view0?.rule_level === "merge-request-only" ? Math.max(view0.required_approvals, 1) : 0;
  const approvalsMet = !approvalsNeeded || (view0?.approvals?.length ?? 0) >= approvalsNeeded;
  const blocking = view0?.blocking || [];
  const blocked = open && blocking.length > 0;

  return (
    <>
      <h1>
        !{view0?.iid} {view0?.title} {view0 && <Pill tone={open ? "warn" : "normal"}>{t(view0.state)}</Pill>}
      </h1>
      <p className="muted">
        <Avatar name={view0?.author || "?"} size={20} /> <code>{repo}</code> · <span className="mono">{view0?.source} → {view0?.target}</span> · {t("opened by {0}", view0?.author || "")}{" "}
        {whenStr(view0?.created_at)}
        {view0?.merge_commit && (
          <>
            {" "}·{" "}
            {t(
              "merged as {0} by {1}",
              <Link className="mono" to={`${base}/commit/${view0.merge_commit}`}>
                {shortOid(view0.merge_commit)}
              </Link>,
              view0.merged_by
            )}
          </>
        )}
      </p>
      {view0?.description && <p className="mergeRequest-desc">{view0.description}</p>}
      {err && <ErrorBox error={err} />}

      <div className="tab-row">
        <button className={"tab" + (tab === "overview" ? " active" : "")} onClick={() => setParams((p) => { const n = new URLSearchParams(p); n.delete("tab"); return n; })}>
          {t("overview")}
        </button>
        <button className={"tab" + (tab === "diff" ? " active" : "")} onClick={() => setParams((p) => { const n = new URLSearchParams(p); n.set("tab", "diff"); return n; })}>
          {t("changes")}
        </button>
      </div>

      {tab === "diff" &&
        (view0?.state !== "open" ? (
          <p className="muted pad">{t("the request is {0} — the change view is the stored record above.", t(view0?.state || "closed"))}</p>
        ) : !view0?.merge_base ? (
          <p className="muted pad">{t("unrelated histories — no merge base to diff against.")}</p>
        ) : (
          <>
            {diff.isPending && <Spinner />}
            {diff.isError && <ErrorBox error={diff.error} />}
            {editingPath && view0 && (
              <Suspense fallback={<Spinner label={t("loading editor…")} />}>
                <MergeRequestFileEdit
                  repo={repo}
                  branch={view0.source}
                  base={view0.source_tip}
                  path={editingPath}
                  defaultMessage={`update ${editingPath} via merge request !${iid}`}
                  onDone={() => {
                    setEditingPath("");
                    invalidate();
                  }}
                />
              </Suspense>
            )}
            {diff.data && (
              <Suspense fallback={<Spinner />}>
                <MergeRequestDiff
                  files={toDiffViewFiles(diff.data)}
                  view={view}
                  onViewChange={setView}
                  onEdit={me ? setEditingPath : undefined}
                  editingPath={editingPath}
                  comments={discussion.data?.comments}
                  sourceTip={view0?.source_tip}
                  canComment={!!me && open && !comment.isPending}
                  onComment={
                    me
                      ? (body, path, side, line, parentID) =>
                          comment.mutate({ body, path, side, line, parentID })
                      : undefined
                  }
                />
              </Suspense>
            )}
            {comment.error && (
              <ErrorBox error={comment.error instanceof Error ? comment.error : String(comment.error)} />
            )}
            {me && open && view0 && (
              <Suspense fallback={null}>
                <MergeRequestReviewBar repo={repo} iid={iid} me={me} author={view0.author} onInvalidate={invalidate} />
              </Suspense>
            )}
          </>
        ))}

      {tab === "overview" && (
        <>
        <div className="merge-request-overview">
          <section>
            <h3>{t("merge status")}</h3>
            {open ? (
              view0?.mergeable ? (
                blocked ? (
                  <div className="error-box">
                    {t("request-changes outstanding from {0} — their re-review is the key, a push never dismisses it.", blocking.join(", "))}
                  </div>
                ) : (
                  <p className="ok-text">{t("mergeable — no conflicts against {0}.", view0.target)}</p>
                )
              ) : (
                <div className="error-box">
                  {t("conflicts: ")}
                  {(view0?.conflicts || []).join(", ") || t("the branches conflict")}
                  {t(" — resolve on the source side, the check is live.")}
                </div>
              )
            ) : (
              <p className="muted">{t(view0?.state || "closed")}.</p>
            )}
            {open && view0?.rule_level && (
              <p className="muted">
                {t("target rule: ")}<code>{view0.rule_level}</code>
                {approvalsNeeded > 0 && t(" — needs {0} approval(s) from others", approvalsNeeded)}
              </p>
            )}
            {open && (
              <p className="act-row">
                <button className="btn primary" disabled={!view0?.mergeable || !approvalsMet || blocked || act.isPending} onClick={() => act.mutate("merge")}>
                  {t("merge")}
                </button>
                {me && me !== view0?.author && (
                  approvedByMe ? (
                    <button className="btn ghost" disabled={act.isPending} onClick={() => act.mutate("unapprove")}>
                      {t("revoke my approval")}
                    </button>
                  ) : (
                    <button className="btn" disabled={act.isPending} onClick={() => act.mutate("approve")}>
                      {t("approve")}
                    </button>
                  )
                )}
                <button className="btn ghost" disabled={act.isPending} onClick={() => act.mutate("close")}>
                  {t("close")}
                </button>
              </p>
            )}
            {view0?.state === "closed" && (
              <p className="act-row">
                <button className="btn" disabled={act.isPending} onClick={() => act.mutate("reopen")}>
                  {t("reopen")}
                </button>
              </p>
            )}
          </section>
          <section>
            <h3>{t("approvals")}</h3>
            {(view0?.approvals || []).length === 0 ? (
              <p className="muted">
                {t("none yet")}
                {approvalsNeeded > 0 ? t(" — {0} needed", approvalsNeeded) : ""}.
              </p>
            ) : (
              <ul className="approval-list">
                {(view0?.approvals || []).map((u) => (
                  <li key={u}>
                    <Avatar name={u} size={20} ring="ok" />
                    <span className="mono">{u}</span>
                  </li>
                ))}
              </ul>
            )}
            {approvalsNeeded > 0 && (
              <p className="muted">
                {t("{0}/{1} — the author's own approval never counts.", (view0?.approvals || []).length, approvalsNeeded)}
              </p>
            )}
          </section>
        </div>
        {view0 && (
          <Suspense fallback={<Spinner />}>
            <MergeRequestTimeline
              repo={repo}
              iid={iid}
              mergeRequest={{
                author: view0.author,
                created_at: view0.created_at,
                state: view0.state,
                merged_by: view0.merged_by || undefined,
                updated_at: view0.updated_at,
              }}
              discussion={discussion.data}
              sourceTip={view0.source_tip}
              canAct={!!me}
              onInvalidate={invalidate}
            />
          </Suspense>
        )}
        </>
      )}
    </>
  );
}
