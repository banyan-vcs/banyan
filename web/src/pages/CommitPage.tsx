// CommitPage.tsx — commit metadata plus the unified diff against its first
// parent (GET /api/v1/diff, byte-pinned against git diff).
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { api, apiText, qs } from "../api/client";
import { parseUnifiedDiff } from "../api/diff";
import type { LogResponse } from "../api/types";
import { DiffView } from "../components/DiffView";
import { ErrorBox, Spinner, whenStr } from "../components/ui";
import { useRepoBase, useRepoId } from "../repo";
import { useT } from "../i18n";

export function CommitPage(): ReactNode {
  const t = useT();
  const { oid = "" } = useParams();
  const repo = useRepoId();
  const base = useRepoBase();
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
          {diff.isSuccess && <DiffView files={parseUnifiedDiff(diff.data)} />}
          <p className="pad">
            <Link to={`${base}/history`}>{t("← commits")}</Link>
          </p>
        </>
      )}
    </>
  );
}
