// SettingsPage.tsx — the repository settings area's general page
// (/r/<repo>/settings): size, the display one-liner, the group label,
// rename and a repack trigger (all require the admin rank; a 403
// downgrades the page to read-only). The guards live one step deeper —
// Branch Protection at /r/<repo>/settings/branch-protection — and the
// member table is a first-class destination of its own (/r/<repo>/members).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import type { GroupsResponse } from "../api/groups";
import { can, useMode } from "../mode";
import { useT } from "../i18n";
import { ErrorBox, Spinner, sizeStr } from "../components/ui";
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
        {t("General")} · <code>{repo}</code>
      </h1>
      {error && <div className="error-box">{error}</div>}
      {note && <div className="ok-box">{note}</div>}

      <section>
        <h2>{t("Size")}</h2>
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
        <h2>{t("Description")}</h2>
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

      {can(mode, "groups") && (
        <section>
          <h2>{t("Group")}</h2>
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
        <h2>{t("Rename")}</h2>
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
        <h2>{t("Repack")}</h2>
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
