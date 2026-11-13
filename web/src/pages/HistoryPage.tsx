// HistoryPage.tsx — branch history with cursor paging (from=<last oid>) and
// an optional path filter ("/a/b" keeps commits whose subtree changed).
import { useInfiniteQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { api, qs } from "../api/client";
import type { LogResponse } from "../api/types";
import { ErrorBox, Spinner, shortOid, whenStr } from "../components/ui";

const PAGE = 30;

export function HistoryPage(): ReactNode {
  const { repo = "", ref: branch = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const pathFilter = params.get("path") || "";

  const history = useInfiniteQuery({
    queryKey: ["log", repo, branch, pathFilter],
    queryFn: ({ pageParam }) =>
      api<LogResponse>(`/api/v1/log${qs({ repo, rev: branch, path: pathFilter, limit: PAGE, from: pageParam })}`),
    initialPageParam: "",
    getNextPageParam: (last) =>
      last.entries.length === PAGE ? last.entries[last.entries.length - 1].oid : undefined,
  });
  const entries = history.data?.pages.flatMap((p) => p.entries) || [];

  return (
    <>
      <h1>
        history{branch ? <> · <code>{branch}</code></> : null}
      </h1>
      <form
        className="path-filter"
        onSubmit={(e) => {
          e.preventDefault();
          const input = (e.currentTarget.elements.namedItem("path") as HTMLInputElement).value.trim();
          const next = new URLSearchParams(params);
          if (input) next.set("path", input);
          else next.delete("path");
          setParams(next, { replace: true });
        }}
      >
        <input name="path" placeholder="filter by path (e.g. /a/b — only commits touching that subtree)" defaultValue={pathFilter} />
        <button className="btn ghost" type="submit">
          filter
        </button>
      </form>
      {history.isPending && <Spinner />}
      {history.isError && <ErrorBox error={history.error} />}
      {history.isSuccess && entries.length === 0 && (
        <p className="muted pad">no commits{pathFilter ? ` touching ${pathFilter}` : ""}.</p>
      )}
      {entries.length > 0 && (
        <table>
          <tbody>
            {entries.map((e) => (
              <tr key={e.oid}>
                <td>
                  <Link to={`/r/${repo}/commit/${e.oid}`}>{e.subject}</Link>
                  <div className="muted">
                    {e.author} · {whenStr(e.when)} · {shortOid(e.oid)}
                    {e.parents > 1 ? " · merge" : ""}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {history.hasNextPage && (
        <p className="pad">
          <button className="btn ghost" onClick={() => history.fetchNextPage()} disabled={history.isFetchingNextPage}>
            {history.isFetchingNextPage ? "loading…" : "load more"}
          </button>
        </p>
      )}
    </>
  );
}
