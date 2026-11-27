// MrDiff.tsx — the MR shell over the shared DiffBrowser: every file that
// exists on the source side carries an edit affordance — the inline edit
// that lands on the source branch right from the change view — and, when
// the page hands the discussion over, every line grows the review "+" :
// clicking it opens the thread that lives at (path, side, line) — heads
// anchored to the CURRENT source tip inline, stale threads stay in the
// overview timeline wearing their badge — plus the reply box that posts a
// new anchored comment. The diff surface itself (highlighting, diffstat,
// split/unified, folding) lives in DiffBrowser, shared with the commit page.
import { useState, type ReactNode } from "react";
import type { DiffViewFile } from "../api/diffview";
import type { MRComment } from "../api/mrs";
import { DiffBrowser, type LineWidgetCtx } from "./DiffBrowser";
import { Avatar } from "./Avatar";
import { useT } from "../i18n";
import { whenStr } from "./ui";

// LineThreadBox is the widget row's content: the anchored threads at this
// line (fresh ones only — stale heads never inline-render) and the comment
// form that posts a new head or a reply.
function LineThreadBox({
  threads,
  canComment,
  onComment,
  onClose,
}: {
  threads: MRComment[]; // heads at this line, fresh (anchor == live tip)
  canComment: boolean;
  onComment: (body: string, parentID: number) => void;
  onClose: () => void;
}): ReactNode {
  const t = useT();
  const [draft, setDraft] = useState("");
  // Opening a thread that already lives here starts in REPLY mode (the
  // GitHub posture — the mockup's thread box); an empty line starts a head.
  const [replyTo, setReplyTo] = useState(threads.length > 0 ? threads[0].id : 0); // 0 = a new head
  return (
    <div className="diff-comment-box">
      {threads.map((head) => (
        <div className="dc-thread" key={head.id}>
          <div className="dc-head">
            <Avatar name={head.author} size={16} />
            <span className="mono">{head.author}</span>
            <span className="muted">{whenStr(head.created_at)}</span>
            {head.resolved && <span className="chip resolved-badge">{t("resolved")}</span>}
          </div>
          <div className="dc-body">{head.body}</div>
        </div>
      ))}
      {canComment && (
        <form
          className="dc-form"
          onSubmit={(e) => {
            e.preventDefault();
            if (!draft.trim()) return;
            onComment(draft.trim(), replyTo);
            setDraft("");
            onClose();
          }}
        >
          <textarea
            autoFocus
            rows={2}
            value={draft}
            placeholder={replyTo ? t("reply…") : t("comment on this line…")}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
                e.currentTarget.form?.requestSubmit();
              }
              if (e.key === "Escape") onClose();
            }}
          />
          <p className="act-row">
            {threads.length > 0 && (
              <button type="button" className="btn ghost small" onClick={() => setReplyTo(replyTo ? 0 : threads[0].id)}>
                {replyTo ? t("start a new thread") : t("reply")}
              </button>
            )}
            <span className="flex-sp" />
            <button type="button" className="btn ghost small" onClick={onClose}>
              {t("cancel")}
            </button>
            <button className="btn small" type="submit" disabled={!draft.trim()}>
              {t("comment")}
            </button>
          </p>
        </form>
      )}
    </div>
  );
}

export function MrDiff({
  files,
  view,
  onViewChange,
  onEdit,
  editingPath,
  comments,
  sourceTip,
  canComment,
  onComment,
}: {
  files: DiffViewFile[];
  view: "split" | "unified";
  onViewChange?: (v: "split" | "unified") => void;
  onEdit?: (path: string) => void;
  editingPath?: string;
  // The discussion (all comments) + the live source tip: fresh anchored
  // heads inline-render; replies ride their heads.
  comments?: MRComment[];
  sourceTip?: string;
  canComment?: boolean;
  onComment?: (body: string, path: string, side: "old" | "new", line: number, parentID: number) => void;
}): ReactNode {
  // replies index: parent id → rows (for showing a thread's tail inline).
  const replies = new Map<number, MRComment[]>();
  for (const c of comments || []) {
    if (c.parent_id) replies.set(c.parent_id, [...(replies.get(c.parent_id) || []), c]);
  }
  return (
    <DiffBrowser
      files={files}
      view={view}
      onViewChange={onViewChange}
      editingPath={editingPath}
      emptyHint="the diff is empty — the source carries nothing new against the merge base."
      fileExtra={
        onEdit
          ? (f, path) => {
              if (f.binary || f.newPath === "/dev/null") return null;
              const editing = editingPath === path;
              return (
                <button className="btn ghost small" onClick={() => onEdit(path)} disabled={editing}>
                  {editing ? "editing below" : "edit"}
                </button>
              );
            }
          : undefined
      }
      renderLineWidget={
        canComment || (comments?.length ?? 0) > 0
          ? (path: string, ctx: LineWidgetCtx) => {
              const heads = (comments || []).filter(
                (c) =>
                  !c.parent_id &&
                  c.kind === "comment" &&
                  c.path === path &&
                  c.side === ctx.side &&
                  c.line === ctx.lineNumber &&
                  (!sourceTip || !c.anchor_oid || c.anchor_oid === sourceTip),
              );
              if (!canComment && heads.length === 0) return null;
              return (
                <LineThreadBox
                  threads={heads}
                  canComment={!!canComment}
                  onComment={(body, parentID) => onComment?.(body, path, ctx.side, ctx.lineNumber, parentID)}
                  onClose={ctx.onClose}
                />
              );
            }
          : undefined
      }
    />
  );
}
