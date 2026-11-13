// TreePage.tsx — the code browser: one lazy directory level at a time
// (GET /api/v1/tree) with breadcrumbs and a branch switcher; selecting a
// file shows its content (BlobView) with a blame entry point.
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { api, qs } from "../api/client";
import { branchesOf, listRefs } from "../api/gitrefs";
import type { TreeResponse } from "../api/types";
import { BlobView } from "../components/Blob";
import { FileIcon } from "../components/FileIcon";
import { RepoTabs } from "../components/RepoTabs";
import { ErrorBox, Pill, Spinner, sizeStr } from "../components/ui";

interface ResolveResult {
  type: "blob" | "tree";
  oid: string;
  size: number;
}

export function TreePage(): ReactNode {
  const { repo = "", "*": splat = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const path = splat.replace(/^\/+|\/+$/g, "");
  const refs = useQuery({ queryKey: ["refs", repo], queryFn: () => listRefs(repo), retry: false });
  const branches = branchesOf(refs.data || { refs: [], capabilities: "", defaultBranch: "" });
  const ref = params.get("ref") || refs.data?.defaultBranch || branches[0]?.name || "";

  const tree = useQuery({
    queryKey: ["tree", repo, ref, path],
    queryFn: () => api<TreeResponse>(`/api/v1/tree${qs({ repo, rev: ref, path })}`),
    enabled: ref !== "",
    retry: false,
  });
  // a 404 from the tree listing may just mean "this path is a file"
  const resolved = useQuery({
    queryKey: ["resolve", repo, ref, path],
    queryFn: () => api<ResolveResult>("/api/v1/resolve", { method: "POST", body: JSON.stringify({ repo, rev: ref, path }) }),
    enabled: ref !== "" && path !== "" && tree.isError,
    retry: false,
  });
  const blob = useQuery({
    queryKey: ["object", repo, resolved.data?.oid],
    queryFn: () => api<ArrayBuffer>(`/api/v1/objects${qs({ repo, oid: resolved.data?.oid || "" })}`),
    enabled: resolved.data?.type === "blob",
    retry: false,
  });

  const crumbs = path === "" ? [] : path.split("/");
  const parent = crumbs.slice(0, -1).join("/");

  return (
    <>
      <h1>
        <code>{repo}</code>
        <select
          className="ref-select"
          value={ref}
          onChange={(e) => {
            const next = new URLSearchParams(params);
            next.set("ref", e.target.value);
            setParams(next, { replace: true });
          }}
        >
          {branches.map((b) => (
            <option key={b.name} value={b.name}>
              {b.name}
              {b.isDefault ? " (default)" : ""}
            </option>
          ))}
        </select>
      </h1>
      <RepoTabs repo={repo} current="" />
      <div className="crumbs">
        <Link to={`/r/${repo}/tree${ref ? `?ref=${encodeURIComponent(ref)}` : ""}`}>{repo}</Link>
        {crumbs.map((seg, i) => {
          const upto = crumbs.slice(0, i + 1).join("/");
          return (
            <span key={upto}>
              {" / "}
              <Link to={`/r/${repo}/tree/${upto}${ref ? `?ref=${encodeURIComponent(ref)}` : ""}`}>{seg}</Link>
            </span>
          );
        })}
        {ref && <Pill>{ref}</Pill>}
      </div>

      {tree.isPending && ref !== "" && <Spinner />}
      {tree.isSuccess && (
        <table>
          <thead>
            <tr>
              <th>name</th>
              <th>size</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {path !== "" && (
              <tr>
                <td colSpan={3}>
                  <Link to={`/r/${repo}/tree/${parent}${ref ? `?ref=${encodeURIComponent(ref)}` : ""}`} className="muted">
                    ../
                  </Link>
                </td>
              </tr>
            )}
            {tree.data.entries.map((e) => (
              <tr key={e.name}>
                <td>
                  <span className={"entry-icon " + e.type}>
                    <FileIcon name={e.name} kind={e.type} mode={e.mode} />
                  </span>{" "}
                  {e.type === "tree" ? (
                    <Link to={`/r/${repo}/tree/${path ? path + "/" : ""}${e.name}${ref ? `?ref=${encodeURIComponent(ref)}` : ""}`}>
                      {e.name}
                    </Link>
                  ) : (
                    <Link to={`/r/${repo}/tree/${path ? path + "/" : ""}${e.name}${ref ? `?ref=${encodeURIComponent(ref)}` : ""}`}>
                      {e.name}
                    </Link>
                  )}
                </td>
                <td className="muted">{e.size !== undefined ? sizeStr(e.size) : e.type === "commit" ? "submodule" : "—"}</td>
                <td>
                  {e.type === "blob" && ref && (
                    <Link
                      className="muted rowlink"
                      to={`/r/${repo}/blame/${path ? path + "/" : ""}${e.name}?ref=${encodeURIComponent(ref)}`}
                    >
                      blame
                    </Link>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {tree.isError && path !== "" && (
        <>
          {resolved.isPending && <Spinner label="resolving…" />}
          {resolved.data?.type === "blob" && (
            <>
              {blob.isPending && <Spinner label="loading file…" />}
              {blob.isError && <ErrorBox error={blob.error} />}
              {blob.isSuccess && (
                <BlobView name={crumbs[crumbs.length - 1] || ""} bytes={new Uint8Array(blob.data as ArrayBuffer)} size={resolved.data.size} />
              )}
            </>
          )}
          {(resolved.isError || resolved.data?.type === "tree") && <ErrorBox error={tree.error} />}
        </>
      )}
    </>
  );
}
