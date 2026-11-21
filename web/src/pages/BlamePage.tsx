// BlamePage.tsx — blame a file at a ref: GET /api/v1/blame returns
// --line-porcelain output byte-pinned against git blame.
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { apiText, qs } from "../api/client";
import { parseBlame } from "../api/blame";
import { BlameView } from "../components/BlameView";
import { ErrorBox, Spinner } from "../components/ui";
import { useRepoBase, useRepoId } from "../repo";
import { useT } from "../i18n";

export function BlamePage(): ReactNode {
  const t = useT();
  const { "*": splat = "" } = useParams();
  const repo = useRepoId();
  const base = useRepoBase();
  const [params] = useSearchParams();
  const path = splat.replace(/^\/+/, "");
  const ref = params.get("ref") || "";

  const blame = useQuery({
    queryKey: ["blame", repo, ref, path],
    queryFn: () => apiText(`/api/v1/blame${qs({ repo, rev: ref, path })}`),
    enabled: ref !== "" && path !== "",
    retry: false,
  });

  return (
    <>
      <h1>
        {t("blame")} · <code>{path}</code>
        {ref ? <span className="muted"> @ {ref}</span> : null}
      </h1>
      <p className="pad">
        <Link to={`${base}/tree/${path}${ref ? `?ref=${encodeURIComponent(ref)}` : ""}`}>{t("← back to the file")}</Link>
      </p>
      {blame.isPending && <Spinner />}
      {blame.isError && <ErrorBox error={blame.error} />}
      {blame.isSuccess && <BlameView lines={parseBlame(blame.data)} />}
    </>
  );
}
