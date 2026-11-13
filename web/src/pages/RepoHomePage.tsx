// RepoHomePage.tsx — one repo's landing page: the branch list (THE USER'S
// filtered view, parsed from the same ref advertisement git sees) and the
// default branch's recent history.
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { api, qs } from "../api/client";
import { branchesOf, listRefs } from "../api/gitrefs";
import type { LogResponse } from "../api/types";
import { ErrorBox, Pill, Spinner, shortOid, whenStr } from "../components/ui";

export function RepoHomePage(): ReactNode {
  const { repo = "" } = useParams();
  const [params] = useSearchParams();
  const refs = useQuery({ queryKey: ["refs", repo], queryFn: () => listRefs(repo), retry: false });
  const branches = branchesOf(refs.data || { refs: [], capabilities: "", defaultBranch: "" });
  const defaultBranch = refs.data?.defaultBranch || branches[0]?.name || "";

  const log = useQuery({
    queryKey: ["log", repo, defaultBranch, ""],
    queryFn: () => api<LogResponse>(`/api/v1/log${qs({ repo, rev: defaultBranch, limit: 10 })}`),
    enabled: defaultBranch !== "",
    retry: false,
  });

  const cloneBase = params.get("clone") || "";

  return (
    <>
      <h1>
        <code>{repo}</code>
        {defaultBranch && <Pill>default: {defaultBranch}</Pill>}
      </h1>
      {cloneBase === "" && null}
      <div className="two-col">
        <section>
          <h2>branches · your view</h2>
          {refs.isPending && <Spinner />}
          {refs.isError && <ErrorBox error={refs.error} />}
          {refs.isSuccess && branches.length === 0 && <p className="muted pad">no branches yet — push one.</p>}
          {branches.length > 0 && (
            <table>
              <thead>
                <tr>
                  <th>branch</th>
                  <th>head</th>
                </tr>
              </thead>
              <tbody>
                {branches.map((b) => (
                  <tr key={b.name}>
                    <td>
                      <Link to={`/r/${repo}/tree?ref=${encodeURIComponent(b.name)}`}>{b.name}</Link>
                      {b.isDefault && <Pill>default</Pill>}
                    </td>
                    <td>
                      <Link to={`/r/${repo}/commit/${b.oid}`} className="muted mono">
                        {shortOid(b.oid)}
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <p className="muted pad">
            personal branches of other users are simply not advertised here — the same filtered ref list your git client gets.
          </p>
        </section>
        <section>
          <h2>recent history{defaultBranch ? ` · ${defaultBranch}` : ""}</h2>
          {log.isPending && <Spinner />}
          {log.isError && <p className="muted pad">no history yet.</p>}
          {log.isSuccess && (
            <table>
              <tbody>
                {log.data.entries.map((e) => (
                  <tr key={e.oid}>
                    <td>
                      <Link to={`/r/${repo}/commit/${e.oid}`}>{e.subject}</Link>
                      <div className="muted">
                        {e.author} · {whenStr(e.when)} · {shortOid(e.oid)}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {defaultBranch && (
            <p className="pad">
              <Link to={`/r/${repo}/history/${defaultBranch}`}>full history →</Link>
            </p>
          )}
        </section>
      </div>
    </>
  );
}
