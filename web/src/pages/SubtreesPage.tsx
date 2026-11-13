// SubtreesPage.tsx — the directory-timeline management view: every
// directory that owns a history, branch heads with their last commits, the
// projected pill (refs/banyan/tree/<dir>), per-path activity, the two
// clone recipes, and the runtime access rules (admin).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api, qs } from "../api/client";
import { useSession } from "../session";
import type { LogResponse, SubtreeListing, SubtreeRule, SubtreeSummary } from "../api/types";
import { Empty, ErrorBox, Pill, Spinner, shortOid, whenStr } from "../components/ui";

export function SubtreesPage(): ReactNode {
  const { repo = "" } = useParams();
  const qc = useQueryClient();
  const { info } = useSession();
  const [params, setParams] = useSearchParams();
  const selected = params.get("path") || "";
  const [ruleUsers, setRuleUsers] = useState("");
  const [error, setError] = useState("");

  const listing = useQuery({
    queryKey: ["subtrees", repo],
    queryFn: () => api<SubtreeListing>(`/api/v1/subtrees${qs({ repo })}`),
    retry: false,
  });
  // GitLab-profile display names (hashed repo id → root/project); empty
  // in the standalone profile — same cache the repos page rides.
  const names = useQuery({
    queryKey: ["gitlab-names"],
    queryFn: () => api<{ names: Record<string, string> }>("/api/gitlab-names"),
    retry: false,
  });
  const rules = useQuery({
    queryKey: ["subtree-rules", repo],
    queryFn: () => api<{ rules: SubtreeRule[] }>(`/api/v1/repos/${encodeURIComponent(repo)}/subtree-access`),
    retry: false,
  });
  const admin = rules.isSuccess;
  const sub: SubtreeSummary | undefined = listing.data?.subtrees.find((s) => s.path === selected);
  // projection refs are per top-level directory — /frameworks/base rides
  // refs/banyan/tree/frameworks (docs/design/data-model.md §12)
  const top = sub ? sub.path.replace(/^\//, "").split("/")[0] : "";
  const activity = useQuery({
    queryKey: ["log", repo, "", selected, "activity"],
    queryFn: () => api<LogResponse>(`/api/v1/log${qs({ repo, limit: 10, path: selected })}`),
    enabled: selected !== "",
    retry: false,
  });

  const ruleOf = (path: string): SubtreeRule | undefined => (rules.data?.rules || []).find((r) => r.path === path);
  const putRule = useMutation({
    mutationFn: (r: { path: string; users: string[] }) =>
      api(`/api/v1/repos/${encodeURIComponent(repo)}/subtree-access`, { method: "PUT", body: JSON.stringify(r) }),
    onSuccess: () => {
      setError("");
      qc.invalidateQueries({ queryKey: ["subtree-rules", repo] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  const delRule = useMutation({
    mutationFn: (path: string) =>
      api(`/api/v1/repos/${encodeURIComponent(repo)}/subtree-access?path=${encodeURIComponent(path)}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["subtree-rules", repo] }),
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  const select = (path: string): void => {
    const next = new URLSearchParams(params);
    if (path) next.set("path", path);
    else next.delete("path");
    setParams(next, { replace: true });
  };

  return (
    <>
      <h1>
        subtrees · <code>{repo}</code>
      </h1>
      <p className="muted">every directory that owns a commit history. <code>projected</code> marks the projection refs (refs/banyan/tree/&lt;dir&gt;) — one URL, one credential per subtree.</p>
      {error && <div className="error-box">{error}</div>}
      {listing.isPending && <Spinner />}
      {listing.isError && <ErrorBox error={listing.error} />}
      {listing.isSuccess && listing.data.subtrees.length === 0 && <Empty>no directory timelines yet — push into a subdirectory.</Empty>}
      {listing.isSuccess && listing.data.subtrees.length > 0 && (
        <div className="two-col">
          <table>
            <thead>
              <tr>
                <th>path</th>
                <th>branches</th>
              </tr>
            </thead>
            <tbody>
              {listing.data.subtrees.map((s) => (
                <tr key={s.path} className={s.path === selected ? "selected" : ""}>
                  <td>
                    <a
                      href="#"
                      onClick={(e) => {
                        e.preventDefault();
                        select(s.path === selected ? "" : s.path);
                      }}
                    >
                      {s.path}
                    </a>
                    {s.projected && <Pill>projected</Pill>}
                    {ruleOf(s.path) && <Pill tone="warn">restricted</Pill>}
                  </td>
                  <td className="muted">{s.branches.map((b) => b.name).join(", ")}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <section>
            {!sub && <p className="muted pad">select a subtree to see its activity{admin ? " and access rule" : ""}.</p>}
            {sub && (
              <>
                <h2>{sub.path}</h2>
                <h3>branch heads</h3>
                <table>
                  <tbody>
                    {sub.branches.map((b) => (
                      <tr key={b.name}>
                        <td>
                          <code>{b.name}</code>
                          <div className="muted">
                            {b.subject || ""} · {b.author || ""} {b.when ? `· ${whenStr(b.when)}` : ""}
                          </div>
                        </td>
                        <td className="muted mono">{shortOid(b.head)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                <h3>clone</h3>
                {info?.provider?.kind === "gitlab" && (names.data?.names[repo] || "").includes("/") && (
                  <>
                    <p className="muted">via the GitLab URL (one URL, one credential):</p>
                    <pre>
                      <code>{`git clone http://<gitlab-host>/${names.data!.names[repo]}.git
git fetch origin refs/banyan/tree/${top}:refs/heads/${top}
git checkout ${top}          # workspace = that subtree only
# work, commit, then push back:
git push origin ${top}:refs/banyan/tree/${top}`}</code>
                    </pre>
                  </>
                )}
                <p className="muted">via banyan's own face:</p>
                <pre>
                  <code>{`git clone http://<banyan>/r/${repo}.git/tree${sub.path}
# work, commit, then push back:
git push origin ${listing.data?.defaultBranch || "main"}`}</code>
                </pre>
                <h3>activity</h3>
                {activity.isPending && <Spinner />}
                {activity.isSuccess && activity.data.entries.length === 0 && <p className="muted pad">no history at this path yet.</p>}
                {activity.isSuccess && (
                  <table>
                    <tbody>
                      {activity.data.entries.map((e) => (
                        <tr key={e.oid}>
                          <td>
                            <Link to={`/r/${repo}/commit/${e.oid}`}>{e.subject}</Link>
                            <div className="muted">
                              {e.author} · {whenStr(e.when)}
                            </div>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
                {admin && (
                  <>
                    <h3>access rule</h3>
                    {ruleOf(sub.path) ? (
                      <div className="rule-box">
                        <div>
                          allowed users: <strong>{ruleOf(sub.path)!.users.join(", ") || "(nobody)"}</strong>
                        </div>
                        <button className="btn ghost danger" onClick={() => delRule.mutate(sub.path)}>
                          remove rule
                        </button>
                      </div>
                    ) : (
                      <p className="muted">no rule — inherited from the repo policy.</p>
                    )}
                    <form
                      className="rule-form"
                      onSubmit={(e) => {
                        e.preventDefault();
                        const users = ruleUsers.split(/[\s,]+/).filter(Boolean);
                        if (users.length > 0 || ruleOf(sub.path)) putRule.mutate({ path: sub.path, users });
                      }}
                    >
                      <input
                        placeholder="allowed users (space or comma separated; empty = nobody)"
                        value={ruleUsers}
                        onChange={(e) => setRuleUsers(e.target.value)}
                      />
                      <button className="btn" type="submit" disabled={putRule.isPending}>
                        {ruleOf(sub.path) ? "update rule" : "restrict to users"}
                      </button>
                    </form>
                  </>
                )}
              </>
            )}
          </section>
        </div>
      )}
    </>
  );
}
