// SettingsPage.tsx — repository administration: size, the force-push
// switch, rename and a repack trigger (all require the admin rank; a 403
// downgrades the page to read-only).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import { ErrorBox, Spinner, sizeStr } from "../components/ui";

export function SettingsPage(): ReactNode {
  const { repo = "" } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const [to, setTo] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");

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
