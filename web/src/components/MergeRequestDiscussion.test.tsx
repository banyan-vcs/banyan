// MergeRequestDiscussion.test.tsx — the review mechanics' pure seams: the timeline
// fold (events / reviews / threads in one ascending order, replies tucked
// under their heads) and the staleness rule (an anchored thread whose tip
// fell behind the live one wears the badge — never a silent move). The
// avatar initial rule rides along: first code point, case kept.
import { describe, expect, it } from "vitest";
import { buildTimeline, threadIsStale } from "./MergeRequestDiscussion";
import { avatarInitial } from "./Avatar";
import type { MergeRequestComment } from "../api/mergerequests";

const c = (over: Partial<MergeRequestComment>): MergeRequestComment => ({
  id: 0,
  repo: "r",
  iid: 1,
  author: "alice",
  body: "",
  kind: "comment",
  resolved: false,
  created_at: 0,
  ...over,
});

describe("buildTimeline", () => {
  it("folds events, reviews, and threads into one ascending order", () => {
    const rows = buildTimeline(
      { author: "alice", created_at: 10, state: "merged", merged_by: "boss", updated_at: 90 },
      {
        comments: [
          c({ id: 2, author: "boss", body: "second", created_at: 30 }),
          c({ id: 3, author: "alice", body: "reply", parent_id: 2, created_at: 40 }),
          c({ id: 4, author: "boss", kind: "review", verdict: "request-changes", body: "hm", created_at: 50 }),
          c({ id: 5, author: "carol", body: "first", created_at: 20 }),
        ],
        approval_events: [{ user: "boss", at: 60 }],
      },
    );
    expect(rows.map((r) => r.kind)).toEqual(["event", "thread", "thread", "review", "event", "event"]);
    expect(rows[0]).toMatchObject({ what: "created", who: "alice" });
    const first = rows[1] as Extract<(typeof rows)[number], { what: "thread" }>;
    expect(first.head.id).toBe(5);
    expect(first.replies).toHaveLength(0);
    const second = rows[2] as Extract<(typeof rows)[number], { what: "thread" }>;
    expect(second.head.id).toBe(2);
    expect(second.replies.map((r) => r.id)).toEqual([3]);
    expect(rows[3]).toMatchObject({ kind: "review", verdict: "request-changes" });
    expect(rows[4]).toMatchObject({ what: "approved", who: "boss" });
    expect(rows[5]).toMatchObject({ what: "merged", who: "boss" });
  });

  it("keeps replies out of the top level and out of the event stream", () => {
    const rows = buildTimeline(
      { author: "a", created_at: 1, state: "open", updated_at: 1 },
      { comments: [c({ id: 1, body: "head", created_at: 2 }), c({ id: 2, body: "reply", parent_id: 1, created_at: 3 })], approval_events: [] },
    );
    expect(rows).toHaveLength(2); // created + one thread
    const thread = rows[1] as Extract<(typeof rows)[number], { what: "thread" }>;
    expect(thread.replies).toHaveLength(1);
  });

  it("never emits a merged event for open or closed requests", () => {
    for (const state of ["open", "closed"]) {
      const rows = buildTimeline(
        { author: "a", created_at: 1, state, updated_at: 2 },
        { comments: [], approval_events: [] },
      );
      expect(rows.filter((r) => r.kind === "event" && r.what === "merged")).toHaveLength(0);
    }
  });
});

describe("threadIsStale", () => {
  it("marks anchored threads whose tip fell behind, and only those", () => {
    const fresh = c({ id: 1, path: "a.go", side: "new", line: 3, anchor_oid: "tip2" });
    const stale = c({ id: 2, path: "a.go", side: "new", line: 3, anchor_oid: "tip1" });
    const plain = c({ id: 3, body: "overview" });
    expect(threadIsStale(fresh, "tip2")).toBe(false);
    expect(threadIsStale(stale, "tip2")).toBe(true);
    expect(threadIsStale(plain, "tip2")).toBe(false);
    expect(threadIsStale(stale, "")).toBe(false); // no live tip to compare
  });
});

describe("avatarInitial", () => {
  it("takes the first code point, case kept", () => {
    expect(avatarInitial("alice")).toBe("a");
    expect(avatarInitial("Bob")).toBe("B");
    expect(avatarInitial("炽天")).toBe("炽");
    expect(avatarInitial("")).toBe("?");
  });
});
