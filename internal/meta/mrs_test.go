package meta

// MR + branch-rule store conformance (docs/design/merge-requests.md): the
// same battery on every backend, per the swappability contract. The IID
// allocation and the optimistic TransitionMR gate are the two behaviors the
// merge path leans on — both must be identical across mem/sqlite/mysql/pg.
import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestMRStoreConformance(t *testing.T) {
	for name, o := range metaOpeners() {
		t.Run(name, func(t *testing.T) {
			ms, done := o(t)
			defer done()
			st, ok := ms.(MRStore)
			if !ok {
				t.Fatalf("backend %s does not implement MRStore", name)
			}
			rs, ok := ms.(BranchRuleStore)
			if !ok {
				t.Fatalf("backend %s does not implement BranchRuleStore", name)
			}
			ctx := context.Background()

			// --- IID allocation: per repo, monotonic ---
			m1, err := st.CreateMR(ctx, MRInfo{Repo: "r", Title: "one", Source: "feat", Target: "main",
				State: MRStateOpen, Author: "alice", SourceHead: "a1", CreatedAt: 1, UpdatedAt: 1})
			if err != nil || m1.IID != 1 {
				t.Fatalf("create 1: iid=%d err=%v", m1.IID, err)
			}
			m2, err := st.CreateMR(ctx, MRInfo{Repo: "r", Title: "two", Source: "feat2", Target: "main",
				State: MRStateOpen, Author: "bob", SourceHead: "b1", CreatedAt: 2, UpdatedAt: 2})
			if err != nil || m2.IID != 2 {
				t.Fatalf("create 2: iid=%d err=%v", m2.IID, err)
			}
			other, err := st.CreateMR(ctx, MRInfo{Repo: "r2", Title: "other", Source: "x", Target: "main",
				State: MRStateOpen, Author: "carol", SourceHead: "c1", CreatedAt: 3, UpdatedAt: 3})
			if err != nil || other.IID != 1 {
				t.Fatalf("create other repo: iid=%d err=%v", other.IID, err)
			}

			// Invalid input bounces before any write.
			if _, err := st.CreateMR(ctx, MRInfo{Repo: "r", Source: "same", Target: "same", State: MRStateOpen, Author: "a", Title: "t"}); err == nil {
				t.Fatal("source == target must be rejected")
			}

			// --- lookup ---
			got, err := st.MRByIID(ctx, "r", 1)
			if err != nil || got.Title != "one" || got.Author != "alice" {
				t.Fatalf("by iid: %+v err=%v", got, err)
			}
			if _, err := st.MRByIID(ctx, "r", 99); !errors.Is(err, ErrMRUnknown) {
				t.Fatalf("unknown mr: %v", err)
			}

			// --- listing: state filter, newest IID first ---
			list, err := st.ListMRs(ctx, "r", MRStateOpen)
			if err != nil || len(list) != 2 || list[0].IID != 2 || list[1].IID != 1 {
				t.Fatalf("list open: %+v err=%v", list, err)
			}
			if list, err = st.ListMRs(ctx, "r", ""); err != nil || len(list) != 2 {
				t.Fatalf("list all: %+v err=%v", list, err)
			}

			// --- optimistic transition: one racer wins ---
			merged, err := st.TransitionMR(ctx, "r", 1, MRStateOpen, func(m *MRInfo) {
				m.State = MRStateMerged
				m.MergeCommit = "mc1"
				m.MergedBy = "carol"
				m.UpdatedAt = 9
			})
			if err != nil || merged.MergeCommit != "mc1" || merged.MergedBy != "carol" {
				t.Fatalf("merge transition: %+v err=%v", merged, err)
			}
			if _, err := st.TransitionMR(ctx, "r", 1, MRStateOpen, func(m *MRInfo) { m.State = MRStateMerged }); !errors.Is(err, ErrMRState) {
				t.Fatalf("second merge must lose the race: %v", err)
			}
			if _, err := st.TransitionMR(ctx, "r", 404, MRStateOpen, func(m *MRInfo) {}); !errors.Is(err, ErrMRUnknown) {
				t.Fatalf("unknown transition: %v", err)
			}
			// Reopen keeps the IID forever (shareable URLs).
			if _, err := st.TransitionMR(ctx, "r", 1, MRStateMerged, func(m *MRInfo) { m.State = MRStateClosed }); err != nil {
				t.Fatalf("close after merge: %v", err)
			}

			// --- approvals ---
			if err := st.AddApproval(ctx, "r", 2, "carol", 5); err != nil {
				t.Fatalf("approve: %v", err)
			}
			if err := st.AddApproval(ctx, "r", 2, "bob", 4); err != nil {
				t.Fatalf("approve 2: %v", err)
			}
			if err := st.AddApproval(ctx, "r", 2, "carol", 6); err != nil { // idempotent upsert
				t.Fatalf("re-approve: %v", err)
			}
			users, err := st.ListApprovals(ctx, "r", 2)
			if err != nil || len(users) != 2 || users[0] != "bob" || users[1] != "carol" {
				t.Fatalf("approvals: %v err=%v", users, err)
			}
			if err := st.RemoveApproval(ctx, "r", 2, "bob"); err != nil {
				t.Fatalf("un-approve: %v", err)
			}
			if err := st.RemoveApproval(ctx, "r", 2, "bob"); err != nil { // idempotent
				t.Fatalf("un-approve again: %v", err)
			}
			if users, err = st.ListApprovals(ctx, "r", 2); err != nil || len(users) != 1 || users[0] != "carol" {
				t.Fatalf("approvals after removal: %v err=%v", users, err)
			}
			if err := st.AddApproval(ctx, "r", 99, "x", 1); !errors.Is(err, ErrMRUnknown) {
				t.Fatalf("approve unknown mr: %v", err)
			}

			// --- branch rules: upsert, hot-path lookup, delete ---
			if err := rs.PutBranchRule(ctx, BranchRule{Repo: "r", Branch: "main", Level: RuleMROnly, Approvals: 1}); err != nil {
				t.Fatalf("put rule: %v", err)
			}
			if err := rs.PutBranchRule(ctx, BranchRule{Repo: "r", Branch: "main", Level: RuleMROnly, Approvals: 2}); err != nil {
				t.Fatalf("upsert rule: %v", err)
			}
			if err := rs.PutBranchRule(ctx, BranchRule{Repo: "r", Branch: "rel", Level: RuleProtected, Approvals: 0}); err != nil {
				t.Fatalf("put rule 2: %v", err)
			}
			if err := rs.PutBranchRule(ctx, BranchRule{Repo: "r", Branch: "bad", Level: "nope", Approvals: 1}); err == nil {
				t.Fatal("bad level must be rejected")
			}
			if err := rs.PutBranchRule(ctx, BranchRule{Repo: "r", Branch: "bad", Level: RuleMROnly, Approvals: 0}); err == nil {
				t.Fatal("mr-only with zero approvals must be rejected")
			}
			rule, ok, err := rs.BranchRuleOf(ctx, "r", "main")
			if err != nil || !ok || rule.Level != RuleMROnly || rule.Approvals != 2 {
				t.Fatalf("rule of: %+v ok=%v err=%v", rule, ok, err)
			}
			if _, ok, err := rs.BranchRuleOf(ctx, "r", "none"); err != nil || ok {
				t.Fatalf("rule of missing: ok=%v err=%v", ok, err)
			}
			rules, err := rs.BranchRules(ctx, "r")
			if err != nil || len(rules) != 2 || rules[0].Branch != "main" || rules[1].Branch != "rel" {
				t.Fatalf("rules list: %+v err=%v", rules, err)
			}
			if err := rs.DeleteBranchRule(ctx, "r", "rel"); err != nil {
				t.Fatalf("delete rule: %v", err)
			}
			if err := rs.DeleteBranchRule(ctx, "r", "rel"); err != nil { // idempotent
				t.Fatalf("delete rule again: %v", err)
			}
			if rules, err = rs.BranchRules(ctx, "r"); err != nil || len(rules) != 1 {
				t.Fatalf("rules after delete: %+v err=%v", rules, err)
			}

			// --- concurrent creation: the IID allocation is serialized under
			// the store's own lock — every racer gets a distinct, positive IID
			// (the SQL store exercises its unique-key retry here). ---
			const racers = 8
			var wg sync.WaitGroup
			iids := make([]int64, racers)
			rerrs := make([]error, racers)
			for i := 0; i < racers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					m, err := st.CreateMR(ctx, MRInfo{Repo: "race", Title: "r", Source: fmt.Sprintf("s%d", i),
						Target: "main", State: MRStateOpen, Author: "a", CreatedAt: 1, UpdatedAt: 1})
					iids[i], rerrs[i] = m.IID, err
				}(i)
			}
			wg.Wait()
			seen := map[int64]bool{}
			for i := 0; i < racers; i++ {
				if rerrs[i] != nil {
					t.Fatalf("racer %d: %v", i, rerrs[i])
				}
				if iids[i] < 1 || seen[iids[i]] {
					t.Fatalf("racer %d: iid %d invalid or duplicated", i, iids[i])
				}
				seen[iids[i]] = true
			}
		})
	}
}
