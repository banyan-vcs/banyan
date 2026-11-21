// FilePanel.tsx — one file's content panel under the tree browser: the
// plain BlobView by default, the Monaco editor as a toggle (full
// tokenization), and an edit mode that commits through the server-side
// assembly (blob upload + POST /api/v1/commit) — the same publish path the
// thin CLI rides, so every protection (protected, mr-only, scopes)
// applies unchanged. Monaco is code-split behind the toggle.
import { useMutation } from "@tanstack/react-query";
import { lazy, Suspense, useState, type ReactNode } from "react";
import { commitFileEdit, EDIT_CAP } from "../api/commitEdit";
import { useT } from "../i18n";
import { BlobView, looksBinary } from "./Blob";
import { Spinner } from "./ui";

const CodeEditor = lazy(() => import("./CodeEditor").then((m) => ({ default: m.CodeEditor })));

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
  const t = useT();
  const [monacoView, setMonacoView] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  const editable = !looksBinary(bytes) && bytes.length <= EDIT_CAP && !!branch && !!headOid;
  const text = new TextDecoder("utf-8", { fatal: false }).decode(bytes);

  const save = useMutation({
    mutationFn: () =>
      commitFileEdit({ repo, branch, base: headOid, path, content: draft, message: message.trim() || `edit ${path}` }),
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
            {monacoView ? t("plain view") : t("editor view")}
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
            {t("edit")}
          </button>
        )}
      </div>
      {error && <div className="error-box">{error}</div>}
      {editing ? (
        <>
          <Suspense fallback={<Spinner label={t("loading editor…")} />}>
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
              placeholder={t("commit message (default: edit {0})", path)}
              value={message}
              onChange={(e) => setMessage(e.target.value)}
            />
            <button className="btn primary" type="submit" disabled={save.isPending}>
              {t("commit to {0}", branch)}
            </button>
            <button className="btn ghost" type="button" onClick={() => setEditing(false)} disabled={save.isPending}>
              {t("cancel")}
            </button>
          </form>
          <p className="muted">
            {t("commits through the server-side assembly (the thin CLI's publish path) — branch protections apply.")}
          </p>
        </>
      ) : monacoView && !looksBinary(bytes) ? (
        <Suspense fallback={<Spinner label={t("loading editor…")} />}>
          <CodeEditor path={name} value={text} readOnly height="60vh" />
        </Suspense>
      ) : (
        <BlobView name={name} bytes={bytes} size={size} />
      )}
    </div>
  );
}
