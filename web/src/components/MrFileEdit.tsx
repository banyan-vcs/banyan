// MrFileEdit.tsx — inline edit of one file straight from the merge
// request's change view: the Monaco editor opens the blob at the MR's
// source tip and saves through the server-side assembly (blob upload +
// commit CAS'd against that tip), so the landing meets the same branch
// protections a push would, and the diff, mergeability and approvals
// recompute live on success — resolving a conflict is a page-side act.
import { useMutation, useQuery } from "@tanstack/react-query";
import { lazy, Suspense, useState, useEffect, type ReactNode } from "react";
import { api } from "../api/client";
import { commitFileEdit, EDIT_CAP } from "../api/commitEdit";
import { looksBinary } from "./Blob";
import { ErrorBox, Spinner } from "./ui";

const CodeEditor = lazy(() => import("./CodeEditor").then((m) => ({ default: m.CodeEditor })));

interface ResolveResult {
  type: "blob" | "tree";
  oid: string;
  size: number;
}

export function MrFileEdit({
  repo,
  branch,
  base,
  path,
  defaultMessage,
  onDone,
}: {
  repo: string;
  branch: string; // the MR's source branch — where the edit lands
  base: string; // the source tip the commit CASes against
  path: string;
  defaultMessage: string;
  onDone: () => void;
}): ReactNode {
  // The queries key on the CAS base (the source tip): every landed edit
  // advances it, so the next edit of the same path fetches fresh content
  // instead of hitting the stale cache.
  const resolved = useQuery({
    queryKey: ["mr-edit-resolve", repo, branch, base, path],
    queryFn: () =>
      api<ResolveResult>("/api/v1/resolve", { method: "POST", body: JSON.stringify({ repo, rev: branch, path }) }),
    retry: false,
  });
  const blob = useQuery({
    queryKey: ["mr-edit-blob", repo, base, resolved.data?.oid],
    queryFn: () => api<ArrayBuffer>(`/api/v1/objects?repo=${encodeURIComponent(repo)}&oid=${resolved.data?.oid || ""}`),
    enabled: resolved.data?.type === "blob",
    retry: false,
  });

  const [draft, setDraft] = useState("");
  const [message, setMessage] = useState("");
  const [loadedFor, setLoadedFor] = useState("");
  const bytes = blob.data ? new Uint8Array(blob.data as ArrayBuffer) : null;
  // Initialize the draft when the blob arrives (once per path).
  useEffect(() => {
    if (blob.isSuccess && bytes && loadedFor !== path) {
      setDraft(new TextDecoder("utf-8", { fatal: false }).decode(bytes));
      setLoadedFor(path);
    }
  }, [blob.isSuccess, bytes, path, loadedFor]);

  const save = useMutation({
    mutationFn: () =>
      commitFileEdit({ repo, branch, base, path, content: draft, message: message.trim() || defaultMessage }),
    onSuccess: onDone,
  });

  if (resolved.isPending) return <Spinner label="loading file…" />;
  if (resolved.isError) return <ErrorBox error={resolved.error} />;
  if (resolved.data.type !== "blob")
    return (
      <p className="muted pad">
        {path} is not a file on {branch}.
      </p>
    );
  if (blob.isPending) return <Spinner label="loading file…" />;
  if (blob.isError) return <ErrorBox error={blob.error} />;
  if (!bytes || looksBinary(bytes) || bytes.length > EDIT_CAP)
    return (
      <p className="muted pad">
        {path} is not editable in place (binary or over {Math.round(EDIT_CAP / 1024)} KiB) — edit via clone.
      </p>
    );

  return (
    <div className="file-panel mr-edit">
      <div className="file-panel-bar">
        <span className="muted mono">{path}</span>
        <span className="spacer" />
        <span className="muted">commits to {branch}</span>
      </div>
      <Suspense fallback={<Spinner label="loading editor…" />}>
        <CodeEditor path={path.split("/").pop() || path} value={draft} readOnly={false} onChange={setDraft} height="55vh" />
      </Suspense>
      {save.error && <ErrorBox error={save.error} />}
      <form
        className="edit-commit"
        onSubmit={(e) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <input
          placeholder={`commit message (default: ${defaultMessage})`}
          value={message}
          onChange={(e) => setMessage(e.target.value)}
        />
        <button className="btn primary" type="submit" disabled={save.isPending}>
          commit to {branch}
        </button>
        <button className="btn ghost" type="button" onClick={onDone} disabled={save.isPending}>
          cancel
        </button>
      </form>
      <p className="muted">
        the edit rides the server-side assembly (the thin CLI's publish path) — branch protections apply, and the
        diff and mergeability recompute the moment it lands.
      </p>
    </div>
  );
}
