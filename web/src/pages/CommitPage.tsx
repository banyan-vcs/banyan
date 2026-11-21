// CommitPage.tsx — commit metadata plus the diff against its first parent
// (GET /api/v1/diff, byte-pinned against git diff), browsed on the shared
// GitHub-style DiffBrowser — split/unified with the MR change view's
// memory, syntax highlighting, per-file diffstat and folding.
import { useQuery } from "@tanstack/react-query";
import { lazy, Suspense, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { api, apiText, qs } from "../api/client";
import { DIFF_VIEW_KEY, toDiffViewFiles } from "../api/diffview";
import type { LogResponse } from "../api/types";
import { ErrorBox, Spinner, whenStr } from "../components/ui";
import { useRepoBase, useRepoId } from "../repo";
import { useT } from "../i18n";

// The diff browser (highlight.js grammars included) is the heaviest leaf
// in the bundle — it code-splits behind the diff, exactly like the MR face.
const DiffBrowser = lazy(() => import("../components/DiffBrowser").then((m) => ({ default: m.DiffBrowser })));

export function CommitPage(): ReactNode {
  const t = useT();
  const { oid = "" } = useParams();
  const repo = useRepoId();
  const base = useRepoBase();
  // One split/unified memory across the diff surfaces; the commit page
  // defaults to unified (git's own presentation), the MR face to split.
  const [view, setView] = useState<"split" | "unified">(() =>
    localStorage.getItem(DIFF_VIEW_KEY) === "split" ? "split" : "unified",
  );
  const pickView = (v: "split" | "unified"): void => {
    localStorage.setItem(DIFF_VIEW_KEY, v);
    setView(v);
  };
  const commit = useQuery({
    queryKey: ["log", repo, "", "", oid],
    queryFn: () => api<LogResponse>(`/api/v1/log${qs({ repo, from: oid, limit: 1 })}`),
    retry: false,
  });
  const entry = commit.data?.entries[0];
  const parent = entry?.parentOids?.[0] || "";

  const diff = useQuery({
    queryKey: ["diff", repo, parent, oid],
    queryFn: () => apiText(`/api/v1/diff${qs({ repo, from: parent, to: oid })}`),
    enabled: !!entry && parent !== "",
    retry: false,
  });

  return (
    <>
      <h1>
        <code>{oid.slice(0, 12)}</code>
      </h1>
      {commit.isPending && <Spinner />}
      {commit.isError && <ErrorBox error={commit.error} />}
      {commit.isSuccess && !entry && <p className="muted pad">{t("commit not found.")}</p>}
      {entry && (
        <>
          <div className="commit-meta">
            <div className="commit-subject">{entry.subject}</div>
            <div className="muted">
              {entry.author} · {whenStr(entry.when)}
              {entry.parents > 1 ? ` · ${t("merge ({0} parents)", entry.parents)}` : ""}
              {entry.parentOids?.map((p) => (
                <span key={p}>
                  {" · "}{t("parent")}{" "}
                  <Link to={`${base}/commit/${p}`} className="mono">
                    {p.slice(0, 8)}
                  </Link>
                </span>
              ))}
            </div>
          </div>
          {parent === "" && <p className="muted pad">{t("root commit — no parent to diff against.")}</p>}
          {diff.isPending && <Spinner label={t("diffing…")} />}
          {diff.isError && <ErrorBox error={diff.error} />}
          {diff.isSuccess && (
            <Suspense fallback={<Spinner />}>
              <DiffBrowser files={toDiffViewFiles(diff.data)} view={view} onViewChange={pickView} />
            </Suspense>
          )}
          <p className="pad">
            <Link to={`${base}/history`}>{t("← commits")}</Link>
          </p>
        </>
      )}
    </>
  );
}
