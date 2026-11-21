// SettingsPage.tsx — repository administration: size, the force-push
// switch, branch protection rules, rename and a repack trigger (all
// require the admin rank; a 403 downgrades the page to read-only).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import { branchesOf, listRefs } from "../api/gitrefs";
import type { GroupsResponse } from "../api/groups";
import { getRepoMembers, setRepoMember } from "../api/members";
import { branchRules, deleteBranchRule, putBranchRule } from "../api/mrs";
import { can, useMode } from "../mode";
import { useT } from "../i18n";
import { ErrorBox, Pill, Spinner, sizeStr } from "../components/ui";
import { useRepoId } from "../repo";

export function SettingsPage(): ReactNode {
  const t = useT();
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
  // The member ledger (docs/design/data-model.md §16): the effective table
  // (inherited rows marked) over the direct one; writes are write-rank and
  // the store bounces direct grants not strictly above the inherited rank.
  const [memberUser, setMemberUser] = useState("");
  const [memberRank, setMemberRank] = useState("viewer");
  const membersQ = useQuery({
    queryKey: ["repo-members", repo],
    queryFn: () => getRepoMembers(repo),
    retry: false,
  });
  const putMember = useMutation({
    mutationFn: (m: { user: string; rank: string }) => setRepoMember(repo, m.user, m.rank),
    onSuccess: () => {
      setMemberUser("");
      qc.invalidateQueries({ queryKey: ["repo-members", repo] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
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
        t(
          "repacked: {0} → {1} packs, {2} objects, {3} → {4}{5}",
          res.PacksBefore,
          res.PacksAfter,
          res.Objects,
          sizeStr(res.BytesBefore),
          sizeStr(res.BytesAfter),
          res.Delta ? " (delta-encoded)" : ""
        )
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
        {t("settings")} · <code>{repo}</code>
      </h1>
      {error && <div className="error-box">{error}</div>}
      {note && <div className="ok-box">{note}</div>}

      <section>
        <h2>{t("size")}</h2>
        {size.isPending && <Spinner />}
        {size.isSuccess && (
          <p className="pad">
            {size.data.packs === 1
              ? t("{0} across {1} pack", sizeStr(size.data.bytes), 1)
              : t("{0} across {1} packs", sizeStr(size.data.bytes), size.data.packs)}
          </p>
        )}
      </section>

      <section>
        <h2>{t("description")}</h2>
        <p className="muted">{t("the one-liner the repository list shows (truncated to the column's width).")}</p>
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
              placeholder={t("what this repository is")}
              value={desc ?? descQuery.data.description}
              onChange={(e) => setDesc(e.target.value)}
            />
            <button className="btn" type="submit" disabled={putDesc.isPending || desc === null}>
              {t("save")}
            </button>
          </form>
        )}
      </section>

      <section>
        <h2>{t("force-push switch")}</h2>
        <p className="muted">{t("the subtree rollback guard is default-on; enabling rewrites is an explicit administrative opt-in.")}</p>
        {size.isPending && <Spinner />}
        {adminBlocked(force) && <p className="muted pad">{t("admin required.")}</p>}
        {force.isSuccess && (
          <p className="pad">
            <button className={"btn" + (force.data.enabled ? " danger" : "")} disabled={setForce.isPending} onClick={() => setForce.mutate(!force.data.enabled)}>
              {force.data.enabled ? t("disable force-push") : t("enable force-push")}
            </button>{" "}
            {t("currently ")}<strong>{force.data.enabled ? t("enabled") : t("disabled")}</strong>
          </p>
        )}
      </section>

      <section>
        <h2>{t("branch protection")}</h2>
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

      {can(mode, "groups") && (
        <section>
          <h2>{t("members")}</h2>
          <p className="muted">
            {t(
              "direct members plus the grants inherited from the group (marked) — the effective rank is the higher of the two; a direct grant must rank strictly above the inherited one."
            )}
          </p>
          {membersQ.isPending && <Spinner />}
          {membersQ.isError && <ErrorBox error={membersQ.error} />}
          {membersQ.isSuccess && membersQ.data.members.length === 0 && (
            <p className="muted pad">{t("no members — every authenticated user has this repository's baseline access.")}</p>
          )}
          {membersQ.isSuccess && membersQ.data.members.length > 0 && (
            <table>
              <thead>
                <tr>
                  <th>{t("user")}</th>
                  <th>{t("rank")}</th>
                  <th>{t("source")}</th>
                  <th>{t("added by")}</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {membersQ.data.members.map((m) => (
                  <tr key={m.user}>
                    <td className="mono">{m.user}</td>
                    <td>
                      <Pill tone={m.rank === "owner" ? "warn" : m.rank === "developer" ? "ok" : "normal"}>{t(m.rank)}</Pill>
                    </td>
                    <td>
                      {m.viaGroup ? (
                        <Link to={`/groups/${encodeURIComponent(m.viaGroup)}`}>
                          <Pill>{t("inherited from {0}", m.viaGroup)}</Pill>
                        </Link>
                      ) : (
                        <span className="muted">{t("direct")}</span>
                      )}
                    </td>
                    <td className="muted">{m.addedBy}</td>
                    <td>
                      {!m.viaGroup && (
                        <button
                          className="btn ghost danger"
                          disabled={putMember.isPending}
                          onClick={() => {
                            if (window.confirm(t("remove member {0}?", m.user))) putMember.mutate({ user: m.user, rank: "" });
                          }}
                        >
                          {t("remove")}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <form
            className="create-branch"
            onSubmit={(e) => {
              e.preventDefault();
              if (memberUser.trim()) putMember.mutate({ user: memberUser.trim(), rank: memberRank });
            }}
          >
            <input placeholder={t("username…")} value={memberUser} onChange={(e) => setMemberUser(e.target.value)} />
            <select value={memberRank} onChange={(e) => setMemberRank(e.target.value)} title={t("rank")}>
              <option value="viewer">{t("viewer")}</option>
              <option value="developer">{t("developer")}</option>
              <option value="owner">{t("owner")}</option>
            </select>
            <button className="btn" type="submit" disabled={putMember.isPending || !memberUser.trim()}>
              {t("add member")}
            </button>
          </form>
        </section>
      )}

      {can(mode, "groups") && (
        <section>
          <h2>{t("group")}</h2>
          <p className="muted">{t("an organizational label only — it folders the repository list and nothing else; the name and every URL stay untouched.")}</p>
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
                <option value="">{t("— no group —")}</option>
                {(groups.data?.groups ?? []).map((g) => (
                  <option key={g.name} value={g.name}>
                    {g.name}
                  </option>
                ))}
              </select>
              <button className="btn" type="submit" disabled={setGroup.isPending}>
                {t("move")}
              </button>
            </form>
          )}
        </section>
      )}

      <section>
        <h2>{t("rename")}</h2>
        <p className="muted">{t("hashed addressing flips the registry alone; named addressing moves storage without touching object ids.")}</p>
        <form
          className="rename-form"
          onSubmit={(e) => {
            e.preventDefault();
            setError("");
            if (to.trim()) {
              if (window.confirm(t("rename {0} → {1}?", repo, to))) rename.mutate();
            }
          }}
        >
          <input placeholder={t("new name")} value={to} onChange={(e) => setTo(e.target.value)} />
          <button className="btn" type="submit" disabled={rename.isPending}>
            {t("rename")}
          </button>
        </form>
      </section>

      <section>
        <h2>{t("repack")}</h2>
        <p className="muted">{t("merges pack fragments and delta-encodes the result (docs/storage/storage.md §5).")}</p>
        <p className="pad">
          <button className="btn ghost" disabled={repack.isPending} onClick={() => repack.mutate()}>
            {repack.isPending ? t("repacking…") : t("repack now")}
          </button>
        </p>
      </section>

      {size.isError && !adminBlocked(size) && <ErrorBox error={size.error} />}
    </>
  );
}
