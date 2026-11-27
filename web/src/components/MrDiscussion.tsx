// MrDiscussion.tsx — the MR's review mechanics on the page
// (docs/design/merge-requests.md §review): the timeline (events + threads
// in one order) and the conclusion bar. Threads anchor display on the
// head's anchor_oid: when it matches the live source tip the file:line
// chip is live; when the source has advanced past it the thread wears the
// dashed 已过时 badge and stays put — threads never silently move. A
// resolved thread collapses to its summary line; the original stays
// readable one click away. The conclusion bar submits one verdict; the
// author's only verdict is comment (approve and request-changes are other
// people's by construction), and ⌘/Ctrl-Enter files whatever is typed.
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import type { MRComment, MRDiscussion } from "../api/mrs";
import { resolveMRComment, submitMRReview } from "../api/mrs";
import { Avatar } from "./Avatar";
import { useT } from "../i18n";
import { whenStr } from "./ui";

// TimelineRow is one renderable row: an event (created / approved /
// reviewed / merged) or a thread (head + replies).
export type TimelineRow =
  | { kind: "event"; at: number; who: string; what: "created" | "approved" | "merged"; verdict?: never }
  | { kind: "review"; at: number; who: string; what: "review"; verdict: string; body: string }
  | { kind: "thread"; at: number; who: string; what: "thread"; head: MRComment; replies: MRComment[] };

// buildTimeline folds the discussion into the rendered order: events from
// the ledger's shape (reviews are events, not threads), threads as heads
// with their replies tucked in, everything ascending by time. Exported for
// the vitest battery.
export function buildTimeline(mr: {
  author: string;
  created_at: number;
  state: string;
  merged_by?: string;
  updated_at: number;
}, d: MRDiscussion): TimelineRow[] {
  const rows: TimelineRow[] = [{ kind: "event", at: mr.created_at, who: mr.author, what: "created" }];
  for (const ev of d.approval_events || []) {
    rows.push({ kind: "event", at: ev.at, who: ev.user, what: "approved" });
  }
  const replies = new Map<number, MRComment[]>();
  for (const c of d.comments || []) {
    if (c.parent_id) {
      replies.set(c.parent_id, [...(replies.get(c.parent_id) || []), c]);
    }
  }
  for (const c of d.comments || []) {
    if (c.kind === "review") {
      rows.push({ kind: "review", at: c.created_at, who: c.author, what: "review", verdict: c.verdict || "comment", body: c.body });
      continue;
    }
    if (!c.parent_id) {
      rows.push({ kind: "thread", at: c.created_at, who: c.author, what: "thread", head: c, replies: replies.get(c.id) || [] });
    }
  }
  if (mr.state === "merged" && mr.merged_by) {
    rows.push({ kind: "event", at: mr.updated_at, who: mr.merged_by, what: "merged" });
  }
  rows.sort((a, b) => a.at - b.at);
  return rows;
}

// threadIsStale answers whether an anchored thread's tip is behind the
// live one — the dashed badge, never a silent move.
export function threadIsStale(head: MRComment, sourceTip: string): boolean {
  return !!head.anchor_oid && !!sourceTip && head.anchor_oid !== sourceTip;
}

const VERDICT_TONE: Record<string, string> = {
  approve: "ok",
  "request-changes": "err",
  comment: "",
};

function EventRow({ row }: { row: Extract<TimelineRow, { what: "created" | "approved" | "merged" }> }): ReactNode {
  const t = useT();
  const label =
    row.what === "created"
      ? t("opened this merge request")
      : row.what === "approved"
        ? t("approved")
        : t("merged this merge request");
  return (
    <li className="mr-ev">
      <Avatar name={row.who} size={16} />
      <span className="mono ev-who">{row.who}</span>
      <span className="muted">{label}</span>
      <span className="muted ev-at">{whenStr(row.at)}</span>
    </li>
  );
}

function ReviewEventRow({ row }: { row: Extract<TimelineRow, { what: "review" }> }): ReactNode {
  const t = useT();
  const tone = VERDICT_TONE[row.verdict] || "";
  const label =
    row.verdict === "approve"
      ? t("approved — request changes with a new review to withdraw")
      : row.verdict === "request-changes"
        ? t("requested changes — this blocks the merge until re-reviewed")
        : t("reviewed");
  return (
    <li className="mr-ev review">
      <Avatar name={row.who} size={16} />
      <span className="mono ev-who">{row.who}</span>
      <span className={"ev-verdict " + tone}>{label}</span>
      <span className="muted ev-at">{whenStr(row.at)}</span>
      {row.body && <p className="ev-body">{row.body}</p>}
    </li>
  );
}

function ThreadRow({
  row,
  sourceTip,
  canAct,
  onResolve,
}: {
  row: Extract<TimelineRow, { what: "thread" }>;
  sourceTip: string;
  canAct: boolean;
  onResolve: (id: number, resolved: boolean) => void;
}): ReactNode {
  const t = useT();
  const { head, replies } = row;
  const [open, setOpen] = useState(true);
  const stale = threadIsStale(head, sourceTip);
  const anchored = !!head.anchor_oid;
  if (head.resolved && !open) {
    return (
      <li className="mr-thread resolved-collapsed">
        <button className="thread-summary" onClick={() => setOpen(true)}>
          <span className="resolved-dot" aria-hidden="true" />
          {t("{0} — resolved by {1} · {2} reply(ies)", head.author, head.resolved_by || "", replies.length)}
          <span className="muted"> · {t("show")}</span>
        </button>
      </li>
    );
  }
  return (
    <li className={"mr-thread" + (head.resolved ? " resolved" : "") + (stale ? " stale" : "")}>
      <div className="thread-head">
        <Avatar name={head.author} size={20} />
        <span className="mono">{head.author}</span>
        <span className="muted">{whenStr(head.created_at)}</span>
        {anchored && (
          <span className="chip chip-fileline mono">
            {head.path}:{head.side === "old" ? "−" : "+"}
            {head.line}
          </span>
        )}
        {stale && <span className="chip stale-badge">{t("outdated")}</span>}
        {head.resolved && <span className="chip resolved-badge">{t("resolved")}</span>}
        {canAct && (
          <button className="btn ghost small" onClick={() => onResolve(head.id, !head.resolved)}>
            {head.resolved ? t("unresolve") : t("resolve")}
          </button>
        )}
        {head.resolved && (
          <button className="btn ghost small" onClick={() => setOpen(false)}>
            {t("collapse")}
          </button>
        )}
      </div>
      <div className="thread-body">{head.body}</div>
      {replies.map((r) => (
        <div className="thread-reply" key={r.id}>
          <Avatar name={r.author} size={16} />
          <span className="mono">{r.author}</span>
          <span className="muted">{whenStr(r.created_at)}</span>
          <div className="thread-body">{r.body}</div>
        </div>
      ))}
    </li>
  );
}

export function MrTimeline(props: {
  repo: string;
  iid: number | string;
  mr: { author: string; created_at: number; state: string; merged_by?: string; updated_at: number };
  discussion?: MRDiscussion;
  sourceTip: string;
  canAct: boolean;
  onInvalidate: () => void;
}): ReactNode {
  const qc = useQueryClient();
  const resolve = useMutation({
    mutationFn: (a: { id: number; resolved: boolean }) => resolveMRComment(props.repo, props.iid, a.id, a.resolved),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mr-discussion", props.repo, String(props.iid)] });
      props.onInvalidate();
    },
  });
  const rows = props.discussion ? buildTimeline(props.mr, props.discussion) : [];
  const t = useT();
  return (
    <section className="mr-discussion">
      <h3>{t("discussion")}</h3>
      {rows.length === 0 ? (
        <p className="muted">{t("no discussion yet — comments, replies and verdicts land here.")}</p>
      ) : (
        <ul className="mr-timeline">
          {rows.map((row, i) =>
            row.kind === "event" ? (
              <EventRow key={"e" + i} row={row} />
            ) : row.kind === "review" ? (
              <ReviewEventRow key={"r" + i} row={row} />
            ) : (
              <li key={"t" + row.head.id} className="thread-li">
                <ThreadRow
                  row={row}
                  sourceTip={props.sourceTip}
                  canAct={props.canAct && !resolve.isPending}
                  onResolve={(id, resolved) => resolve.mutate({ id, resolved })}
                />
              </li>
            ),
          )}
        </ul>
      )}
    </section>
  );
}

// MrReviewBar — the conclusion bar under the change view: one verdict, an
// optional body, one submission. The author's verdict set is comment only.
export function MrReviewBar({
  repo,
  iid,
  me,
  author,
  onInvalidate,
}: {
  repo: string;
  iid: number | string;
  me: string;
  author: string;
  onInvalidate: () => void;
}): ReactNode {
  const t = useT();
  const qc = useQueryClient();
  const isAuthor = me === author;
  const [verdict, setVerdict] = useState<"approve" | "request-changes" | "comment">("comment");
  const [body, setBody] = useState("");
  const submit = useMutation({
    mutationFn: () => submitMRReview(repo, iid, verdict, body),
    onSuccess: () => {
      setBody("");
      qc.invalidateQueries({ queryKey: ["mr-discussion", repo, String(iid)] });
      onInvalidate();
    },
  });
  const err = submit.error ? (submit.error instanceof Error ? submit.error.message : String(submit.error)) : "";
  const choices: { v: "approve" | "request-changes" | "comment"; label: string; tone: string }[] = [
    { v: "approve", label: t("approve"), tone: "ok" },
    { v: "request-changes", label: t("request changes"), tone: "err" },
    { v: "comment", label: t("comment only"), tone: "" },
  ];
  return (
    <section className="mr-review-bar">
      <Avatar name={me} size={26} />
      <div className="review-bar-main">
        <div className="verdict-row" role="radiogroup" aria-label={t("review verdict")}>
          {choices.map((c) => (
            <label key={c.v} className={"verdict-choice " + c.tone + (verdict === c.v ? " picked" : "")}>
              <input
                type="radio"
                name="verdict"
                checked={verdict === c.v}
                disabled={isAuthor && c.v !== "comment"}
                onChange={() => setVerdict(c.v)}
              />
              {c.label}
            </label>
          ))}
        </div>
        {isAuthor && <p className="muted">{t("the author's only verdict is a comment — approvals come from others.")}</p>}
        <textarea
          value={body}
          placeholder={t("leave a note with the verdict (optional)")}
          onChange={(e) => setBody(e.target.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === "Enter" && !submit.isPending) {
              submit.mutate();
            }
          }}
          rows={2}
        />
        {err && <div className="error-box">{err}</div>}
        <p className="act-row">
          <button className="btn primary" disabled={submit.isPending} onClick={() => submit.mutate()}>
            {t("submit review")}
          </button>
          <span className="muted">{t("⌘/Ctrl-Enter files the review")}</span>
        </p>
      </div>
    </section>
  );
}
