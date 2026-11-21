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
import { approveMR, closeMR, getMR, mergeMR, reopenMR, unapproveMR } from "../api/mrs";
// The diff view (highlight.js grammars included) is the heaviest leaf in
// the bundle — it code-splits behind the changes tab, as does the editor
// the inline edit opens.
const MrDiff = lazy(() => import("../components/MrDiff").then((m) => ({ default: m.MrDiff })));
const MrFileEdit = lazy(() => import("../components/MrFileEdit").then((m) => ({ default: m.MrFileEdit })));
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
      const msg = what === "merge" && view0?.state === "open" ? window.prompt(t("merge commit message (empty = default)")) ?? null : undefined;
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
          <p className="muted pad">{t("no merge request !{0} in {1} — IIDs are never reused, so this link points at nothing.", iid, repo)}</p>
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
        !{view0?.iid} {view0?.title} {view0 && <Pill tone={open ? "warn" : "normal"}>{t(view0.state)}</Pill>}
      </h1>
      <p className="muted">
        <code>{repo}</code> · <span className="mono">{view0?.source} → {view0?.target}</span> · {t("opened by {0}", view0?.author || "")}{" "}
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
      {view0?.description && <p className="mr-desc">{view0.description}</p>}
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
                <MrFileEdit
                  repo={repo}
                  branch={view0.source}
                  base={view0.source_tip}
                  path={editingPath}
                  defaultMessage={`update ${editingPath} via MR !${iid}`}
                  onDone={() => {
                    setEditingPath("");
                    invalidate();
                  }}
                />
              </Suspense>
            )}
            {diff.data && (
              <Suspense fallback={<Spinner />}>
                <MrDiff
                  files={toDiffViewFiles(diff.data)}
                  view={view}
                  onViewChange={setView}
                  onEdit={me ? setEditingPath : undefined}
                  editingPath={editingPath}
                />
              </Suspense>
            )}
          </>
        ))}

      {tab === "overview" && (
        <div className="mr-overview">
          <section>
            <h3>{t("merge status")}</h3>
            {open ? (
              view0?.mergeable ? (
                <p className="ok-text">{t("mergeable — no conflicts against {0}.", view0.target)}</p>
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
                <button className="btn primary" disabled={!view0?.mergeable || !approvalsMet || act.isPending} onClick={() => act.mutate("merge")}>
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
                  <li key={u}>{u}</li>
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
      )}
    </>
  );
}
