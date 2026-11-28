// One-command demonstration seeding (config `demo: true`): a fresh
// deployment boots into a populated world — known demo accounts, a
// playground repository whose README is the guide, two directory-timeline
// subtrees, three merge requests (open+mergeable, conflicting, merged), an
// merge-request-only release rule, a personal branch, a group with inherited member
// grants. Everything rides the SAME service APIs the console and the thin
// CLI use (PutObjects → CreateCommit, OpenMergeRequest/ApproveMergeRequest/MergeMergeRequest, the
// timeline CAS) — no shell-outs, no wire forgery, so the seed doubles as
// an executable walkthrough of the write path.
//
// Production never runs this (banyand bootstraps its administrator
// instead); re-runs on an already-seeded deployment are a no-op.
package demoseed

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
)

// DemoPassword is the shared password of the seeded demo accounts — a
// demonstration is unusable if its credentials are a secret; the terminal
// names them at boot and the guide repeats them.
const DemoPassword = "demo"

// Seed populates a fresh deployment. Idempotent: a deployment that already
// carries the demo repository is left alone. guideDir, when not empty,
// receives a copy of the guide markdown beside the data (banyand passes
// the storage root).
func Seed(ctx context.Context, svc *service.Service, guideDir string) error {
	if _, err := svc.Meta.GetRepo(ctx, "demo"); err == nil {
		return nil // already seeded
	}
	us, ok := svc.Meta.(meta.UserStore)
	if !ok {
		return fmt.Errorf("demo mode requires the account table (this metadata plane has none)")
	}
	gs, ok := svc.Meta.(meta.GroupStore)
	if !ok {
		return fmt.Errorf("demo mode requires the group store")
	}
	mms, ok := svc.Meta.(meta.MemberStore)
	if !ok {
		return fmt.Errorf("demo mode requires the member store")
	}
	brs, ok := svc.Meta.(meta.BranchRuleStore)
	if !ok {
		return fmt.Errorf("demo mode requires the branch-rule store")
	}

	seedUsers(ctx, us)
	seedAccountsNote()

	seedMembers(ctx, gs, mms)
	if err := svc.Meta.CreateRepo(ctx, "demo", svc.Algo); err != nil {
		return fmt.Errorf("create demo repo: %w", err)
	}
	if err := svc.Meta.SetRepoDescription(ctx, "demo", "the banyan playground — read me first: this README is the tour"); err != nil {
		return err
	}
	if err := gs.SetRepoGroup(ctx, "demo", "platform"); err != nil {
		return err
	}

	// The content, in dependency order: the root commit makes the subtree
	// paths visible, the anchored timelines fork off it, then the branch /
	// merge-request world.
	c := &seeder{svc: svc}
	if err := c.content(ctx); err != nil {
		return err
	}
	if err := brs.PutBranchRule(ctx, meta.BranchRule{Repo: "demo", Branch: "release", Level: meta.RuleMergeRequestOnly, Approvals: 1}); err != nil {
		return err
	}

	if guideDir != "" {
		if err := os.MkdirAll(guideDir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(guideDir, "demo-guide.md"), []byte(Guide), 0o644)
		}
	}
	log.Printf("demo: seeded — accounts admin/alice/bob (password %q), repository demo (README is the guide), subtrees /src /docs, merge requests !1 !2 !3", DemoPassword)
	return nil
}

func seedUsers(ctx context.Context, us meta.UserStore) {
	hash, err := auth.HashPassword(DemoPassword)
	if err != nil {
		log.Printf("demo: cannot hash the demo password: %v", err)
		return
	}
	now := time.Now().Unix()
	for _, u := range []meta.User{
		{Name: "admin", Password: hash, Admin: true, CreatedAt: now},
		{Name: "alice", Password: hash, CreatedAt: now},
		{Name: "bob", Password: hash, CreatedAt: now},
	} {
		if err := us.PutUser(ctx, u); err != nil {
			log.Printf("demo: account %s: %v", u.Name, err)
		}
	}
}

func seedAccountsNote() {
	log.Printf("demo accounts: admin / alice / bob — password %q (change them from Settings → Profile)", DemoPassword)
}

func seedMembers(ctx context.Context, gs meta.GroupStore, mms meta.MemberStore) {
	if err := gs.CreateGroup(ctx, meta.GroupInfo{Name: "platform", Description: "the demo platform group", CreatedBy: "admin", CreatedAt: time.Now().Unix()}); err != nil {
		log.Printf("demo: group: %v", err)
		return
	}
	now := time.Now().Unix()
	for _, m := range []meta.Member{
		{User: "alice", Rank: meta.RankOwner, AddedBy: "admin", AddedAt: now},
		{User: "bob", Rank: meta.RankViewer, AddedBy: "admin", AddedAt: now},
	} {
		if err := mms.SetGroupMember(ctx, "platform", m); err != nil {
			log.Printf("demo: group member %s: %v", m.User, err)
		}
	}
}

// seeder carries the service and drives the write path.
type seeder struct {
	svc *service.Service
}

// put uploads one file's blob and answers its Change.
func (c *seeder) put(ctx context.Context, repo, path, content string) (service.Change, error) {
	raw := []byte(content)
	oid := model.ObjectOID(model.BlobType, raw, c.svc.Algo)
	if err := c.svc.PutObjects(ctx, repo, []service.PutObject{{OID: oid, Type: "blob", Data: raw}}); err != nil {
		return service.Change{}, err
	}
	return service.Change{Path: path, Blob: oid}, nil
}

// commit lands one changeset on a timeline (sub "" = root) and answers the
// new head.
func (c *seeder) commit(ctx context.Context, repo, sub, branch string, base model.OID, message, author string, files map[string]string) (model.OID, error) {
	changes := make([]service.Change, 0, len(files))
	for path, content := range files {
		ch, err := c.put(ctx, repo, path, content)
		if err != nil {
			return "", err
		}
		changes = append(changes, ch)
	}
	res, err := c.svc.CreateCommit(ctx, service.CommitRequest{
		Repo: repo, Sub: sub, Branch: branch, Base: base,
		Message: message, Author: &service.Identity{Name: author, Email: author + "@demo.banyan"},
		Changes: changes,
	})
	if err != nil {
		return "", err
	}
	return res.Head, nil
}

// branchCreate points a new branch at head through the same ref CAS a git
// push rides.
func (c *seeder) branchCreate(ctx context.Context, repo, branch string, head model.OID) error {
	return c.svc.UpdateRef(ctx, service.RefUpdate{Repo: repo, Name: "refs/heads/" + branch, New: head})
}

// anchor forks a directory timeline off the merged view — exactly what the
// first anchored push does (adoptSubtreePush's EnsureTimeline step), after
// which CreateCommit(Sub) is the plain path.
func (c *seeder) anchor(ctx context.Context, repo string, sub model.AnchorPath, branch string) (model.OID, error) {
	curTip, _, err := c.svc.Views.SubtreeHead(ctx, repo, sub, branch)
	if err != nil {
		return "", err
	}
	if curTip.IsZero() {
		return "", fmt.Errorf("subtree %s has no visible content to anchor at (commit a file there first)", sub)
	}
	rootTip, err := c.svc.Views.RootTip(ctx, repo, branch)
	if err != nil {
		return "", err
	}
	tl := model.Timeline{Repo: repo, Anchor: sub, Name: branch}
	if err := c.svc.Meta.EnsureTimeline(ctx, tl, curTip, rootTip); err != nil {
		return "", err
	}
	return curTip, nil
}

func (c *seeder) content(ctx context.Context) error {
	const repo = "demo"

	// --- the root timeline: the guide plus the files that make the
	// subtree paths visible ---
	main1, err := c.commit(ctx, repo, "", "main", "", "seed: the playground", "alice", map[string]string{
		"README.md":     Guide,
		"src/hello.go":  helloGo,
		"docs/notes.md": notesMd,
	})
	if err != nil {
		return fmt.Errorf("root commit: %w", err)
	}

	// --- two directory timelines (the subtree demos): anchored off the
	// merged view, then one local commit each — the subtrees page lists
	// them, the woven root view carries their content ---
	for _, st := range []struct {
		sub     model.AnchorPath
		message string
		path    string
		body    string
	}{
		{"/src", "subtree: add the Python stats tool", "src/stats.py", statsPy},
		{"/docs", "subtree: document the timeline model", "docs/timeline.md", timelineMd},
	} {
		base, err := c.anchor(ctx, repo, st.sub, "main")
		if err != nil {
			return fmt.Errorf("anchor %s: %w", st.sub, err)
		}
		if _, err := c.commit(ctx, repo, string(st.sub), "main", base, st.message, "alice", map[string]string{st.path: st.body}); err != nil {
			return fmt.Errorf("subtree commit %s: %w", st.sub, err)
		}
	}

	// --- the merge-request world ---
	// !1: open and mergeable — one approval on it, merge from the console.
	// New branches are created by the ref CAS first (a git branch), then
	// committed on — CreateCommit only ever advances an existing head.
	if err := c.branchCreate(ctx, repo, "hello", main1); err != nil {
		return fmt.Errorf("branch hello: %w", err)
	}
	if _, err := c.commit(ctx, repo, "", "hello", main1, "add the TypeScript greeter", "alice", map[string]string{"src/greet.ts": greetTs}); err != nil {
		return fmt.Errorf("branch hello commit: %w", err)
	}
	mergeRequest1, err := c.svc.OpenMergeRequest(ctx, repo, "hello", "main", "use the TypeScript greeter", "", "alice")
	if err != nil {
		return fmt.Errorf("open !1: %w", err)
	}
	if err := c.svc.ApproveMergeRequest(ctx, repo, mergeRequest1.IID, "bob", time.Now().Unix()); err != nil {
		return fmt.Errorf("approve !1: %w", err)
	}

	// The conflicting pair: the branch and main both rewrite the notes'
	// opening line after the fork — !2 shows the live conflict banner. The
	// root tip is RE-READ here: the anchored subtree commits above moved
	// the woven main view past main1.
	conflictBase, err := c.svc.Views.RootTip(ctx, repo, "main")
	if err != nil {
		return err
	}
	if err := c.branchCreate(ctx, repo, "conflict", conflictBase); err != nil {
		return fmt.Errorf("branch conflict: %w", err)
	}
	if _, err := c.commit(ctx, repo, "", "conflict", conflictBase, "retitle the notes (branch side)", "bob", map[string]string{
		"docs/notes.md": notesBranchSide,
	}); err != nil {
		return fmt.Errorf("branch conflict commit: %w", err)
	}
	main2, err := c.commit(ctx, repo, "", "main", conflictBase, "retitle the notes (main side)", "alice", map[string]string{
		"docs/notes.md": notesMainSide,
	})
	if err != nil {
		return fmt.Errorf("main retitle: %w", err)
	}
	if _, err := c.svc.OpenMergeRequest(ctx, repo, "conflict", "main", "retitle the working notes", "", "bob"); err != nil {
		return fmt.Errorf("open !2: %w", err)
	}

	// !3: a merged request — branch, open, approve, land.
	if err := c.branchCreate(ctx, repo, "changelog", main2); err != nil {
		return fmt.Errorf("branch changelog: %w", err)
	}
	if _, err := c.commit(ctx, repo, "", "changelog", main2, "start the changelog", "alice", map[string]string{"docs/CHANGELOG.md": changelogMd}); err != nil {
		return fmt.Errorf("branch changelog commit: %w", err)
	}
	mergeRequest3, err := c.svc.OpenMergeRequest(ctx, repo, "changelog", "main", "start the changelog", "", "alice")
	if err != nil {
		return fmt.Errorf("open !3: %w", err)
	}
	if err := c.svc.ApproveMergeRequest(ctx, repo, mergeRequest3.IID, "bob", time.Now().Unix()); err != nil {
		return fmt.Errorf("approve !3: %w", err)
	}
	if _, err := c.svc.MergeMergeRequest(ctx, repo, mergeRequest3.IID, "admin", "", time.Now().Unix()); err != nil {
		return fmt.Errorf("merge !3: %w", err)
	}

	// release under the merge-request-only rule; alice/wip shows the personal
	// namespace (only alice and admins see it).
	mainTip, err := c.svc.Views.RootTip(ctx, repo, "main")
	if err != nil {
		return err
	}
	if err := c.branchCreate(ctx, repo, "release", mainTip); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	// The personal namespace rides the timeline CAS directly — the same
	// landing a git push takes (UpdateRef's single-segment rule covers the
	// console face; personal branches are a wire-native shape).
	wip := model.Timeline{Repo: repo, Anchor: model.RootAnchor, Name: "refs/heads/alice/wip"}
	expected := model.OID("")
	if st, terr := c.svc.Meta.Timeline(ctx, wip); terr == nil {
		expected = st.Head
	}
	if err := c.svc.Meta.CASHead(ctx, wip, expected, mainTip); err != nil {
		return fmt.Errorf("alice/wip: %w", err)
	}
	return nil
}
