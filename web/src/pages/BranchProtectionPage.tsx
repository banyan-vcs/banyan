// BranchProtectionPage.tsx — the repository settings area's protection
// page (/r/<repo>/settings/branch-protection): the force-push switch (the
// subtree rollback guard) and the per-branch protection rules (protected /
// mr-only). Split out of the general settings page — the guards are their
// own concern; both still require the admin rank (a 403 downgrades the
// switch to read-only).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { branchesOf, listRefs } from "../api/gitrefs";
import { branchRules, deleteBranchRule, putBranchRule } from "../api/mrs";
import { useT } from "../i18n";
import { ErrorBox, Spinner } from "../components/ui";
import { api } from "../api/client";
import { useRepoId } from "../repo";

export function BranchProtectionPage(): ReactNode {
  const t = useT();
  const repo = useRepoId();
  const qc = useQueryClient();
  const [error, setError] = useState("");
  const [ruleBranch, setRuleBranch] = useState("");
  const [ruleLevel, setRuleLevel] = useState("mr-only");
  const [ruleApprovals, setRuleApprovals] = useState(1);

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
  const branches = branchesOf(refs.data || { refs: [], capabilities: "", defaultBranch: "" });

  const setForce = useMutation({
    mutationFn: (enabled: boolean) =>
      api(`/api/v1/repos/${encodeURIComponent(repo)}/force-push`, { method: "PUT", body: JSON.stringify({ enabled }) }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["force-push", repo] }),
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
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

  const adminBlocked = (q: { isError: boolean; error: unknown }): boolean => {
    return q.isError && q.error instanceof Error && (q.error.message.includes("403") || q.error.message.includes("FORBIDDEN"));
  };

  return (
    <>
      <h1>
        {t("Branch Protection")} · <code>{repo}</code>
      </h1>
      {error && <div className="error-box">{error}</div>}

      <section>
        <h2>{t("Force-Push Switch")}</h2>
        <p className="muted">{t("the subtree rollback guard is default-on; enabling rewrites is an explicit administrative opt-in.")}</p>
        {refs.isPending && <Spinner />}
        {adminBlocked(force) && <p className="muted pad">{t("admin required.")}</p>}
        {force.isSuccess && (
          <p className="pad">
            <button className={"btn" + (force.data.enabled ? " danger" : "")} disabled={setForce.isPending} onClick={() => setForce.mutate(!force.data.enabled)}>
              {force.data.enabled ? t("disable force-push") : t("enable force-push")}
            </button>{" "}
            {t("currently ")}<strong>{force.data.enabled ? t("enabled") : t("disabled")}</strong>
          </p>
        )}
        {force.isError && !adminBlocked(force) && <ErrorBox error={force.error} />}
      </section>

      <section>
        <h2>{t("Rules")}</h2>
        <p className="muted">
          {t(
            "{0} keeps the direct-push semantics (owner rank); {1} accepts no direct push at all — merge requests with the approval count are the only way in.",
            <code>protected</code>,
            <code>mr-only</code>
          )}
        </p>
        {rules.isPending && <Spinner />}
        {rules.isSuccess && (rules.data || []).length > 0 && (
          <table>
            <thead>
              <tr>
                <th>{t("branch")}</th>
                <th>{t("level")}</th>
                <th>{t("approvals")}</th>
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
                      {t("clear")}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {rules.isSuccess && (rules.data || []).length === 0 && <p className="muted pad">{t("no rules — every branch takes the policy default.")}</p>}
        <form
          className="create-branch"
          onSubmit={(e) => {
            e.preventDefault();
            if (ruleBranch) putRule.mutate();
          }}
        >
          <select value={ruleBranch} onChange={(e) => setRuleBranch(e.target.value)}>
            <option value="">{t("branch…")}</option>
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
              title={t("required approvals")}
            />
          )}
          <button className="btn" type="submit" disabled={putRule.isPending || !ruleBranch}>
            {t("set rule")}
          </button>
        </form>
      </section>
    </>
  );
}
