// FilePanel.tsx — one file's content panel under the tree browser: the
// plain BlobView by default, the Monaco editor as a toggle (full
// tokenization), and an edit mode that commits through the server-side
// assembly (blob upload + POST /api/v1/commit) — the same publish path the
// thin CLI rides, so every protection (protected, mr-only, scopes)
// applies unchanged. Monaco is code-split behind the toggle.
import { useMutation } from "@tanstack/react-query";
import { lazy, Suspense, useState, type ReactNode } from "react";
import { api } from "../api/client";
import { BlobView, looksBinary } from "./Blob";
import { Spinner } from "./ui";

const CodeEditor = lazy(() => import("./CodeEditor").then((m) => ({ default: m.CodeEditor })));

const EDIT_CAP = 256 * 1024; // the wire round-trip is whole-file; past this, edit via clone

async function sha1Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-1", bytes as unknown as ArrayBuffer);
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

function toBase64(bytes: Uint8Array): string {
  let out = "";
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    out += String.fromCharCode(...bytes.subarray(i, i + chunk));
  }
  return btoa(out);
}

export function FilePanel({
  repo,
  branch,
  headOid,
  path,
  name,
  bytes,
  size,
  onSaved,
}: {
  repo: string;
  branch: string;
  headOid: string;
  path: string;
  name: string;
  bytes: Uint8Array;
  size: number;
  onSaved: () => void;
}): ReactNode {
  const [monacoView, setMonacoView] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  const editable = !looksBinary(bytes) && bytes.length <= EDIT_CAP && !!branch && !!headOid;
  const text = new TextDecoder("utf-8", { fatal: false }).decode(bytes);

  const save = useMutation({
    mutationFn: async () => {
      const enc = new TextEncoder();
      const content = enc.encode(draft);
      const header = enc.encode(`blob ${content.length}\0`);
      const both = new Uint8Array(header.length + content.length);
      both.set(header);
      both.set(content);
      const oid = await sha1Hex(both);
      await api("/api/v1/objects/content", {
        method: "POST",
        body: JSON.stringify({ repo, objects: [{ oid, type: "blob", data: toBase64(content) }] }),
      });
      await api("/api/v1/commit", {
        method: "POST",
        body: JSON.stringify({
          repo,
          branch,
          base: headOid,
          message: message.trim() || `edit ${path}`,
          changes: [{ path, blob: oid }],
        }),
      });
    },
    onSuccess: () => {
      setEditing(false);
      setError("");
      onSaved();
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  return (
    <div className="file-panel">
      <div className="file-panel-bar">
        <span className="muted mono">{path}</span>
        <span className="spacer" />
        {!editing && !looksBinary(bytes) && (
          <button className="btn ghost" onClick={() => setMonacoView((v) => !v)}>
            {monacoView ? "plain view" : "editor view"}
          </button>
        )}
        {!editing && editable && (
          <button
            className="btn"
            onClick={() => {
              setDraft(text);
              setEditing(true);
              setMessage("");
              setError("");
            }}
          >
            edit
          </button>
        )}
      </div>
      {error && <div className="error-box">{error}</div>}
      {editing ? (
        <>
          <Suspense fallback={<Spinner label="loading editor…" />}>
            <CodeEditor path={name} value={draft} readOnly={false} onChange={setDraft} height="55vh" />
          </Suspense>
          <form
            className="edit-commit"
            onSubmit={(e) => {
              e.preventDefault();
              save.mutate();
            }}
          >
            <input
              placeholder={`commit message (default: edit ${path})`}
              value={message}
              onChange={(e) => setMessage(e.target.value)}
            />
            <button className="btn primary" type="submit" disabled={save.isPending}>
              commit to {branch}
            </button>
            <button className="btn ghost" type="button" onClick={() => setEditing(false)} disabled={save.isPending}>
              cancel
            </button>
          </form>
          <p className="muted">
            commits through the server-side assembly (the thin CLI's publish path) — branch protections apply.
          </p>
        </>
      ) : monacoView && !looksBinary(bytes) ? (
        <Suspense fallback={<Spinner label="loading editor…" />}>
          <CodeEditor path={name} value={text} readOnly height="60vh" />
        </Suspense>
      ) : (
        <BlobView name={name} bytes={bytes} size={size} />
      )}
    </div>
  );
}
