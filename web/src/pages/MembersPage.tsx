// MembersPage.tsx — the repository's member table as a first-class
// destination (/r/<repo>/members, the sidebar's Members item — lifted out
// of the settings page): the effective table (inherited rows marked with
// their group, direct rows removable) over the direct one; writes are
// write-rank and the store bounces direct grants not strictly above the
// inherited rank (docs/design/data-model.md §16).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { getRepoMembers, setRepoMember } from "../api/members";
import { useT } from "../i18n";
import { ErrorBox, Pill, Spinner } from "../components/ui";
import { useRepoId } from "../repo";

export function MembersPage(): ReactNode {
  const t = useT();
  const repo = useRepoId();
  const qc = useQueryClient();
  const [error, setError] = useState("");
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
      setError("");
      qc.invalidateQueries({ queryKey: ["repo-members", repo] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  return (
    <>
      <h1>
        {t("Members")} · <code>{repo}</code>
      </h1>
      <p className="muted">
        {t(
          "direct members plus the grants inherited from the group (marked) — the effective rank is the higher of the two; a direct grant must rank strictly above the inherited one."
        )}
      </p>
      {error && <div className="error-box">{error}</div>}
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
    </>
  );
}
