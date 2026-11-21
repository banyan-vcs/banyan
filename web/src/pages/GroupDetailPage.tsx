// GroupDetailPage.tsx — one group at /groups/<name>: the member table (what
// every labeled repository inherits) and the group's repositories. Members
// are the permission ledger's group side (docs/design/data-model.md §16):
// viewer reads, developer pushes, owner manages — repositories carry the
// grants of their group plus any strictly-higher direct grants of their own.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import { getGroupMembers, setGroupMember } from "../api/members";
import { partitionByGroup, type GroupsResponse } from "../api/groups";
import { repoHref } from "../repo";
import type { RepoInfo } from "../api/types";
import { can, useMode } from "../mode";
import { useT } from "../i18n";
import { Empty, ErrorBox, Pill, Spinner, whenStr } from "../components/ui";

const RANKS = ["viewer", "developer", "owner"];

function rankTone(rank: string): "normal" | "warn" | "ok" {
  return rank === "owner" ? "warn" : rank === "developer" ? "ok" : "normal";
}

export function GroupDetailPage(): ReactNode {
  const t = useT();
  const { name = "" } = useParams();
  const mode = useMode();
  const qc = useQueryClient();
  const [user, setUser] = useState("");
  const [rank, setRank] = useState("viewer");
  const [error, setError] = useState("");

  const groups = useQuery({
    queryKey: ["groups"],
    queryFn: () => api<GroupsResponse>("/api/v1/groups"),
    retry: false,
  });
  const info = groups.data?.groups.find((g) => g.name === name);
  const members = useQuery({
    queryKey: ["group-members", name],
    queryFn: () => getGroupMembers(name),
    retry: false,
    enabled: groups.isSuccess,
  });
  const repos = useQuery({
    queryKey: ["repos"],
    queryFn: () => api<{ repos: RepoInfo[] }>("/api/v1/repos"),
  });

  const save = useMutation({
    mutationFn: (m: { user: string; rank: string }) => setGroupMember(name, m.user, m.rank),
    onSuccess: () => {
      setUser("");
      setError("");
      qc.invalidateQueries({ queryKey: ["group-members", name] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  if (groups.isPending) return <Spinner />;
  if (groups.isError) return <ErrorBox error={groups.error} />;
  if (!info) return <p className="muted pad">{t("group not found.")}</p>;

  const assignments = groups.data?.repoGroups ?? {};
  const mine = partitionByGroup(repos.data?.repos ?? [], assignments, [info]).groups[0]?.repos ?? [];

  return (
    <>
      <h1>
        {t("Group")} · <code>{name}</code>
      </h1>
      {info.description && <p className="muted">{info.description}</p>}
      <p className="muted">
        {info.createdBy} · {whenStr(info.createdAt)}
      </p>

      <section>
        <h2>{t("Members")}</h2>
        <p className="muted">
          {t(
            "the group's member table — every labeled repository inherits these grants (the effective rank is the higher of the two)."
          )}
        </p>
        {error && <div className="error-box">{error}</div>}
        {members.isPending && <Spinner />}
        {members.isError && <ErrorBox error={members.error} />}
        {members.isSuccess && members.data.members.length === 0 && (
          <Empty>{t("no members yet — the group's repositories are open to every authenticated user until you add some.")}</Empty>
        )}
        {members.isSuccess && members.data.members.length > 0 && (
          <table>
            <thead>
              <tr>
                <th>{t("user")}</th>
                <th>{t("rank")}</th>
                <th>{t("added by")}</th>
                <th>{t("added")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {members.data.members.map((m) => (
                <tr key={m.user}>
                  <td className="mono">{m.user}</td>
                  <td>
                    <Pill tone={rankTone(m.rank)}>{t(m.rank)}</Pill>
                  </td>
                  <td className="muted">{m.addedBy}</td>
                  <td className="muted">{whenStr(m.addedAt)}</td>
                  <td>
                    <button
                      className="btn ghost danger"
                      disabled={save.isPending}
                      onClick={() => {
                        if (window.confirm(t("remove member {0}?", m.user))) save.mutate({ user: m.user, rank: "" });
                      }}
                    >
                      {t("remove")}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {can(mode, "groups") && (
          <form
            className="create-branch"
            onSubmit={(e) => {
              e.preventDefault();
              if (user.trim()) save.mutate({ user: user.trim(), rank });
            }}
          >
            <input placeholder={t("username…")} value={user} onChange={(e) => setUser(e.target.value)} />
            <select value={rank} onChange={(e) => setRank(e.target.value)}>
              {RANKS.map((r) => (
                <option key={r} value={r}>
                  {t(r)}
                </option>
              ))}
            </select>
            <button className="btn" type="submit" disabled={save.isPending || !user.trim()}>
              {t("add member")}
            </button>
          </form>
        )}
      </section>

      <section>
        <h2>{t("Repositories")}</h2>
        {mine.length === 0 && <Empty>{t("no repositories in this group yet.")}</Empty>}
        {mine.length > 0 && (
          <table>
            <tbody>
              {mine.map((r) => (
                <tr key={r.id}>
                  <td>
                    <Link to={repoHref(r.id, name)} className="rowlink">
                      {r.id}
                    </Link>
                  </td>
                  <td className="cell-desc muted" title={r.description}>
                    {r.description || "—"}
                  </td>
                  <td className="muted">{r.updatedAt ? whenStr(r.updatedAt) : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </>
  );
}
