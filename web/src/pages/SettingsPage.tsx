// SettingsPage.tsx — repository administration: size, the force-push
// switch, branch protection rules, rename and a repack trigger (all
// require the admin rank; a 403 downgrades the page to read-only).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import { branchesOf, listRefs } from "../api/gitrefs";
import type { GroupsResponse } from "../api/groups";
import { branchRules, deleteBranchRule, putBranchRule } from "../api/mrs";
import { can, useMode } from "../mode";
import { ErrorBox, Spinner, sizeStr } from "../components/ui";
import { useRepoId } from "../repo";

export function SettingsPage(): ReactNode {
  const repo = useRepoId();
  const nav = useNavigate();
  const qc = useQueryClient();
  const mode = useMode();
  const [to, setTo] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [desc, setDesc] = useState<string | null>(null);

  // The display one-liner (the repository list truncates it): write rank,
  // the group-label precedent.
  const descQuery = useQuery({
    queryKey: ["repo-desc", repo],
    queryFn: () => api<{ repo: string; description: string }>(`/api/v1/repos/${encodeURIComponent(repo)}/description`),
    retry: false,
  });
  const putDesc = useMutation({
    mutationFn: (description: string) =>
      api(`/api/v1/repos/${encodeURIComponent(repo)}/description`, { method: "PUT", body: JSON.stringify({ description }) }),
    onSuccess: () => {
      setError("");
      qc.invalidateQueries({ queryKey: ["repo-desc", repo] });
      qc.invalidateQueries({ queryKey: ["repos"] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  const size = useQuery({
    queryKey: ["size", repo],
    queryFn: () => api<{ repo: string; bytes: number; packs: number }>(`/api/v1/repos/${encodeURIComponent(repo)}/size`),
    retry: false,
  });
  const force = useQuery({
    queryKey: ["force-push", repo],
    queryFn: () => api<{ repo: string; enabled: boolean }>(`/api/v1/repos/${encodeURIComponent(repo)}/force-push`),
    retry: false,
  });
  const rules = useQuery({
    queryKey: ["branch-rules", repo],
    queryFn: () => branchRules(repo),
    retry: false,
  });
  const refs = useQuery({ queryKey: ["refs", repo], queryFn: () => listRefs(repo), retry: false });
  // The group label (organizational only — docs/design/data-model.md §15):
  // read the current label, offer every group, PUT the choice.
  const label = useQuery({
    queryKey: ["repo-group", repo],
    queryFn: () => api<{ repo: string; group: string }>(`/api/v1/repos/${encodeURIComponent(repo)}/group`),
    retry: false,
    enabled: can(mode, "groups"),
  });
  const groups = useQuery({
    queryKey: ["groups"],
    queryFn: () => api<GroupsResponse>("/api/v1/groups"),
    retry: false,
    enabled: can(mode, "groups"),
  });
  const setGroup = useMutation({
    mutationFn: (group: string) =>
      api(`/api/v1/repos/${encodeURIComponent(repo)}/group`, { method: "PUT", body: JSON.stringify({ group }) }),
    onSuccess: () => {
      setError("");
      qc.invalidateQueries({ queryKey: ["repo-group", repo] });
      qc.invalidateQueries({ queryKey: ["repos"] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  const branches = branchesOf(refs.data || { refs: [], capabilities: "", defaultBranch: "" });
  const [ruleBranch, setRuleBranch] = useState("");
  const [ruleLevel, setRuleLevel] = useState("mr-only");
  const [ruleApprovals, setRuleApprovals] = useState(1);

  const putRule = useMutation({
    mutationFn: () => putBranchRule(repo, ruleBranch, ruleLevel, ruleApprovals),
    onSuccess: () => {
      setError("");
      qc.invalidateQueries({ queryKey: ["branch-rules", repo] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  const delRule = useMutation({
    mutationFn: (branch: string) => deleteBranchRule(repo, branch),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["branch-rules", repo] }),
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  const setForce = useMutation({
    mutationFn: (enabled: boolean) =>
      api(`/api/v1/repos/${encodeURIComponent(repo)}/force-push`, { method: "PUT", body: JSON.stringify({ enabled }) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["force-push", repo] }),
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  const rename = useMutation({
    mutationFn: () => api(`/api/v1/repos/${encodeURIComponent(repo)}/rename`, { method: "POST", body: JSON.stringify({ to }) }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["repos"] });
      nav(`/r/${to}/settings`);
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  const repack = useMutation({
    mutationFn: () =>
      api<{ PacksBefore: number; PacksAfter: number; Objects: number; BytesBefore: number; BytesAfter: number; Delta: boolean }>(
        "/api/v1/repack",
        { method: "POST", body: JSON.stringify({ repo, threshold: 0 }) }
      ),
    onSuccess: (res) => {
      setNote(
        `repacked: ${res.PacksBefore} → ${res.PacksAfter} packs, ${res.Objects} objects, ` +
          `${sizeStr(res.BytesBefore)} → ${sizeStr(res.BytesAfter)}${res.Delta ? " (delta-encoded)" : ""}`
      );
      qc.invalidateQueries({ queryKey: ["size", repo] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  const adminBlocked = (q: { isError: boolean; error: unknown }): boolean => {
    return q.isError && q.error instanceof Error && (q.error.message.includes("403") || q.error.message.includes("FORBIDDEN"));
  };

  return (
    <>
      <h1>
        settings · <code>{repo}</code>
      </h1>
      {error && <div className="error-box">{error}</div>}
      {note && <div className="ok-box">{note}</div>}

      <section>
        <h2>size</h2>
        {size.isPending && <Spinner />}
        {size.isSuccess && (
          <p className="pad">
            {sizeStr(size.data.bytes)} across {size.data.packs} pack{size.data.packs === 1 ? "" : "s"}
          </p>
        )}
      </section>

      <section>
        <h2>description</h2>
        <p className="muted">the one-liner the repository list shows (truncated to the column's width).</p>
        {descQuery.isSuccess && (
          <form
            className="rule-form"
            onSubmit={(e) => {
              e.preventDefault();
              if (desc !== null) putDesc.mutate(desc);
            }}
          >
            <input
              className="repo-desc-input"
              placeholder="what this repository is"
              value={desc ?? descQuery.data.description}
              onChange={(e) => setDesc(e.target.value)}
            />
            <button className="btn" type="submit" disabled={putDesc.isPending || desc === null}>
              save
            </button>
          </form>
        )}
      </section>

      <section>
        <h2>force-push switch</h2>
        <p className="muted">the subtree rollback guard is default-on; enabling rewrites is an explicit administrative opt-in.</p>
        {force.isPending && <Spinner />}
        {adminBlocked(force) && <p className="muted pad">admin required.</p>}
        {force.isSuccess && (
          <p className="pad">
            <button className={"btn" + (force.data.enabled ? " danger" : "")} disabled={setForce.isPending} onClick={() => setForce.mutate(!force.data.enabled)}>
              {force.data.enabled ? "disable force-push" : "enable force-push"}
            </button>{" "}
            currently <strong>{force.data.enabled ? "enabled" : "disabled"}</strong>
          </p>
        )}
      </section>

      <section>
        <h2>branch protection</h2>
        <p className="muted">
          <code>protected</code> keeps the direct-push semantics (owner rank); <code>mr-only</code> accepts no direct
          push at all — merge requests with the approval count are the only way in.
        </p>
        {rules.isPending && <Spinner />}
        {rules.isSuccess && (rules.data || []).length > 0 && (
          <table>
            <thead>
              <tr>
                <th>branch</th>
                <th>level</th>
                <th>approvals</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {(rules.data || []).map((r) => (
                <tr key={r.branch}>
                  <td className="mono">{r.branch}</td>
                  <td>
                    <code>{r.level}</code>
                  </td>
                  <td>{r.level === "mr-only" ? r.approvals : "—"}</td>
                  <td>
                    <button className="btn ghost danger" disabled={delRule.isPending} onClick={() => delRule.mutate(r.branch)}>
                      clear
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {rules.isSuccess && (rules.data || []).length === 0 && <p className="muted pad">no rules — every branch takes the policy default.</p>}
        <form
          className="create-branch"
          onSubmit={(e) => {
            e.preventDefault();
            if (ruleBranch) putRule.mutate();
          }}
        >
          <select value={ruleBranch} onChange={(e) => setRuleBranch(e.target.value)}>
            <option value="">branch…</option>
            {branches.map((b) => (
              <option key={b.name} value={b.name}>
                {b.name}
              </option>
            ))}
          </select>
          <select value={ruleLevel} onChange={(e) => setRuleLevel(e.target.value)}>
            <option value="mr-only">mr-only</option>
            <option value="protected">protected</option>
          </select>
          {ruleLevel === "mr-only" && (
            <input
              type="number"
              min={1}
              max={9}
              style={{ width: "5em" }}
              value={ruleApprovals}
              onChange={(e) => setRuleApprovals(Number(e.target.value) || 1)}
              title="required approvals"
            />
          )}
          <button className="btn" type="submit" disabled={putRule.isPending || !ruleBranch}>
            set rule
          </button>
        </form>
      </section>

      {can(mode, "groups") && (
        <section>
          <h2>group</h2>
          <p className="muted">an organizational label only — it folders the repository list and nothing else; the name and every URL stay untouched.</p>
          {label.isPending && <Spinner />}
          {label.isSuccess && (
            <form
              className="rule-form"
              onSubmit={(e) => {
                e.preventDefault();
                const v = (e.currentTarget.elements.namedItem("group") as HTMLSelectElement).value;
                setGroup.mutate(v);
              }}
            >
              <select name="group" defaultValue={label.data.group} disabled={setGroup.isPending}>
                <option value="">— no group —</option>
                {(groups.data?.groups ?? []).map((g) => (
                  <option key={g.name} value={g.name}>
                    {g.name}
                  </option>
                ))}
              </select>
              <button className="btn" type="submit" disabled={setGroup.isPending}>
                move
              </button>
            </form>
          )}
        </section>
      )}

      <section>
        <h2>rename</h2>
        <p className="muted">hashed addressing flips the registry alone; named addressing moves storage without touching object ids.</p>
        <form
          className="rename-form"
          onSubmit={(e) => {
            e.preventDefault();
            setError("");
            if (to.trim()) {
              if (window.confirm(`rename ${repo} → ${to}?`)) rename.mutate();
            }
          }}
        >
          <input placeholder="new name" value={to} onChange={(e) => setTo(e.target.value)} />
          <button className="btn" type="submit" disabled={rename.isPending}>
            rename
          </button>
        </form>
      </section>

      <section>
        <h2>repack</h2>
        <p className="muted">merges pack fragments and delta-encodes the result (docs/storage/storage.md §5).</p>
        <p className="pad">
          <button className="btn ghost" disabled={repack.isPending} onClick={() => repack.mutate()}>
            {repack.isPending ? "repacking…" : "repack now"}
          </button>
        </p>
      </section>

      {size.isError && !adminBlocked(size) && <ErrorBox error={size.error} />}
    </>
  );
}
