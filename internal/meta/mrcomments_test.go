package meta

// MR comment store conformance (docs/design/merge-requests.md §review) —
// the same battery on every backend. The behaviors the review path leans
// on: per-request monotonic IDs (the timeline order), reply-only-to-heads,
// resolve-on-heads-only, and the anchor's all-or-nothing shape. The avatar
// column rides along (the users seam's newest column).
import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMRCommentStoreConformance(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			ms, done := o(t)
			defer done()
			st, ok := ms.(MRCommentStore)
			if !ok {
				t.Fatalf("backend %s does not implement MRCommentStore", name)
			}
			mrs, ok := ms.(MRStore)
			if !ok {
				t.Fatalf("backend %s does not implement MRStore", name)
			}
			ctx := context.Background()
			mr1, err := mrs.CreateMR(ctx, MRInfo{Repo: "r", Title: "one", Source: "feat", Target: "main",
				State: MRStateOpen, Author: "alice", CreatedAt: 1, UpdatedAt: 1})
			if err != nil {
				t.Fatalf("create mr: %v", err)
			}
			mr2, err := mrs.CreateMR(ctx, MRInfo{Repo: "r", Title: "two", Source: "feat2", Target: "main",
				State: MRStateOpen, Author: "bob", CreatedAt: 2, UpdatedAt: 2})
			if err != nil {
				t.Fatalf("create mr 2: %v", err)
			}

			// --- unknown request refuses before any write ---
			if _, err := st.CreateMRComment(ctx, MRComment{Repo: "r", IID: 99, Author: "a", Kind: CommentKindComment, Body: "x"}); !errors.Is(err, ErrMRUnknown) {
				t.Fatalf("comment on unknown mr: %v", err)
			}

			// --- validation refusals ---
			bad := []MRComment{
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: CommentKindComment, Body: ""},                        // no body
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: "note", Body: "x"},                                   // unknown kind
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: CommentKindReview, Body: "x"},                        // review without verdict
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: CommentKindReview, Verdict: "maybe", Body: "x"},      // unknown verdict
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: CommentKindComment, Body: "x", Verdict: VerdictApprove}, // verdict on a comment
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: CommentKindReview, Verdict: VerdictApprove, Body: "x", Path: "f.go", Line: 3}, // anchored review
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: CommentKindComment, Body: "x", Line: 3},              // partial anchor
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: CommentKindComment, Body: "x", Path: "f.go", Line: 3}, // anchor without side/oid
				{Repo: "r", IID: mr1.IID, Author: "a", Kind: CommentKindComment, Body: "x", Side: "old"},          // side without line
			}
			for i, c := range bad {
				if _, err := st.CreateMRComment(ctx, c); err == nil {
					t.Fatalf("invalid %d must be rejected: %+v", i, c)
				}
			}

			// --- anchored comment, plain comment, review, reply ---
			anchored, err := st.CreateMRComment(ctx, MRComment{Repo: "r", IID: mr1.IID, Author: "bob",
				Kind: CommentKindComment, Body: "this loop is off by one",
				Path: "cmd/tool/main.go", Line: 27, Side: "new", AnchorOID: "tip1", CreatedAt: 10})
			if err != nil || anchored.ID != 1 {
				t.Fatalf("anchored: id=%d err=%v", anchored.ID, err)
			}
			plain, err := st.CreateMRComment(ctx, MRComment{Repo: "r", IID: mr1.IID, Author: "carol",
				Kind: CommentKindComment, Body: "overview remark", CreatedAt: 11})
			if err != nil || plain.ID != 2 {
				t.Fatalf("plain: id=%d err=%v", plain.ID, err)
			}
			review, err := st.CreateMRComment(ctx, MRComment{Repo: "r", IID: mr1.IID, Author: "carol",
				Kind: CommentKindReview, Verdict: VerdictRequestChanges, Body: "the off-by-one, please", CreatedAt: 12})
			if err != nil || review.ID != 3 {
				t.Fatalf("review: id=%d err=%v", review.ID, err)
			}
			reply, err := st.CreateMRComment(ctx, MRComment{Repo: "r", IID: mr1.IID, Author: "alice", ParentID: anchored.ID,
				Kind: CommentKindComment, Body: "fixed in the next push", CreatedAt: 13})
			if err != nil || reply.ID != 4 {
				t.Fatalf("reply: id=%d err=%v", reply.ID, err)
			}
			// Per-request IDs: the other request restarts at 1.
			other, err := st.CreateMRComment(ctx, MRComment{Repo: "r", IID: mr2.IID, Author: "bob",
				Kind: CommentKindComment, Body: "on the other one", CreatedAt: 14})
			if err != nil || other.ID != 1 {
				t.Fatalf("other request id: %d err=%v", other.ID, err)
			}

			// --- replies only attach to heads ---
			if _, err := st.CreateMRComment(ctx, MRComment{Repo: "r", IID: mr1.IID, Author: "a", ParentID: reply.ID,
				Kind: CommentKindComment, Body: "reply to reply"}); !errors.Is(err, ErrCommentUnknown) {
				t.Fatalf("reply to reply must be refused: %v", err)
			}
			if _, err := st.CreateMRComment(ctx, MRComment{Repo: "r", IID: mr1.IID, Author: "a", ParentID: 404,
				Kind: CommentKindComment, Body: "ghost"}); !errors.Is(err, ErrCommentUnknown) {
				t.Fatalf("reply to missing must be refused: %v", err)
			}

			// --- listing: ID-ascending, unknown request bounces ---
			list, err := st.ListMRComments(ctx, "r", mr1.IID)
			if err != nil || len(list) != 4 {
				t.Fatalf("list: n=%d err=%v", len(list), err)
			}
			for i := 1; i < len(list); i++ {
				if list[i].ID <= list[i-1].ID {
					t.Fatalf("list not ascending: %+v", list)
				}
			}
			if list[0].AnchorOID != "tip1" || list[0].Path != "cmd/tool/main.go" || list[0].Line != 27 || list[0].Side != "new" {
				t.Fatalf("anchor round-trip: %+v", list[0])
			}
			if list[2].Verdict != VerdictRequestChanges || list[2].ParentID != 0 {
				t.Fatalf("review round-trip: %+v", list[2])
			}
			if _, err := st.ListMRComments(ctx, "r", 99); !errors.Is(err, ErrMRUnknown) {
				t.Fatalf("list unknown: %v", err)
			}

			// --- resolve: heads only, idempotent toggle ---
			if err := st.SetMRCommentResolved(ctx, "r", mr1.IID, anchored.ID, true, "alice"); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			list, _ = st.ListMRComments(ctx, "r", mr1.IID)
			if !list[0].Resolved || list[0].ResolvedBy != "alice" {
				t.Fatalf("resolved round-trip: %+v", list[0])
			}
			if err := st.SetMRCommentResolved(ctx, "r", mr1.IID, anchored.ID, false, "alice"); err != nil {
				t.Fatalf("unresolve: %v", err)
			}
			if err := st.SetMRCommentResolved(ctx, "r", mr1.IID, reply.ID, true, "alice"); !errors.Is(err, ErrCommentUnknown) {
				t.Fatalf("resolve a reply must be refused: %v", err)
			}
			if err := st.SetMRCommentResolved(ctx, "r", mr1.IID, 404, true, "alice"); !errors.Is(err, ErrCommentUnknown) {
				t.Fatalf("resolve missing: %v", err)
			}

			// --- approvals with timestamps (the timeline event shape) ---
			if err := mrs.AddApproval(ctx, "r", mr1.IID, "carol", 55); err != nil {
				t.Fatalf("approve: %v", err)
			}
			details, err := mrs.ListApprovalDetails(ctx, "r", mr1.IID)
			if err != nil || len(details) != 1 || details[0].User != "carol" || details[0].At != 55 {
				t.Fatalf("approval details: %+v err=%v", details, err)
			}
			if _, err := mrs.ListApprovalDetails(ctx, "r", 99); !errors.Is(err, ErrMRUnknown) {
				t.Fatalf("details unknown: %v", err)
			}

			// --- concurrent append: every racer a distinct, ascending-slot ID ---
			if _, err := mrs.CreateMR(ctx, MRInfo{Repo: "race", Title: "r", Source: "s", Target: "main",
				State: MRStateOpen, Author: "a", CreatedAt: 1, UpdatedAt: 1}); err != nil {
				t.Fatalf("create race mr: %v", err)
			}
			const racers = 8
			var wg sync.WaitGroup
			ids := make([]int64, racers)
			rerrs := make([]error, racers)
			for i := 0; i < racers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					c, err := st.CreateMRComment(ctx, MRComment{Repo: "race", IID: 1, Author: "a",
						Kind: CommentKindComment, Body: fmt.Sprint(i)})
					ids[i], rerrs[i] = c.ID, err
				}(i)
			}
			wg.Wait()
			seen := map[int64]bool{}
			for i := 0; i < racers; i++ {
				if rerrs[i] != nil {
					t.Fatalf("racer %d: %v", i, rerrs[i])
				}
				if ids[i] < 1 || seen[ids[i]] {
					t.Fatalf("racer %d: id %d invalid or duplicated", i, ids[i])
				}
				seen[ids[i]] = true
			}
		})
	}
}

func TestUserAvatarColumn(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			ms, done := o(t)
			defer done()
			us, ok := ms.(UserStore)
			if !ok {
				t.Fatalf("backend %s does not implement UserStore", name)
			}
			ctx := context.Background()
			if err := us.PutUser(ctx, User{Name: "alice", Password: "h", AvatarURL: "data:image/png;base64,AAAA", CreatedAt: 1}); err != nil {
				t.Fatalf("put with avatar: %v", err)
			}
			got, err := us.UserByName(ctx, "alice")
			if err != nil || got.AvatarURL != "data:image/png;base64,AAAA" {
				t.Fatalf("avatar round-trip: %+v err=%v", got, err)
			}
			if err := us.PutUser(ctx, User{Name: "bob", Password: "h", CreatedAt: 2}); err != nil {
				t.Fatalf("put without avatar: %v", err)
			}
			got, err = us.UserByName(ctx, "bob")
			if err != nil || got.AvatarURL != "" {
				t.Fatalf("empty avatar: %+v err=%v", got, err)
			}
			if err := us.PutUser(ctx, User{Name: "carol", Password: "h", AvatarURL: strings.Repeat("x", MaxAvatarURL+1), CreatedAt: 3}); err == nil {
				t.Fatal("oversized avatar must be rejected")
			}
			if err := us.PutUser(ctx, User{Name: "dave", Password: "h", AvatarURL: "http://plain.example/a.png", CreatedAt: 4}); err == nil {
				t.Fatal("non-https external avatar must be rejected")
			}
			all, err := us.Users(ctx)
			if err != nil || len(all) != 2 {
				t.Fatalf("users: n=%d err=%v", len(all), err)
			}
		})
	}
}
