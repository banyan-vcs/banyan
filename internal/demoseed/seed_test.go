package demoseed

// The seeded world, verified through the same faces the console reads:
// three accounts (one admin, the demo password verifying through bcrypt),
// the demo repository labeled into the platform group with inherited
// member grants, two directory timelines visible as subtrees, three merge
// requests in the three states (one open+clean, one open+conflicting, one
// merged), the merge-request-only release rule, the personal branch — and a re-run is
// a no-op.
import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/banyan-vcs/banyan/internal/auth"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func TestSeed(t *testing.T) {
	// CI runner CPU: hashing at MinCost — see auth.BcryptCost.
	auth.BcryptCost = bcrypt.MinCost
	t.Cleanup(func() { auth.BcryptCost = bcrypt.DefaultCost })
	ms := meta.NewMemoryMetaStore()
	objects := storage.NewObjectService(ms, storage.NewLocalShardStore(t.TempDir()), []storage.Node{{ID: "t1", Local: true}}, model.SHA1)
	svc := service.New(ms, objects, model.SHA1)
	ctx := context.Background()
	require.NoError(t, Seed(ctx, svc, t.TempDir()))

	// --- accounts: the demo password verifies; exactly one admin ---
	var us meta.UserStore = ms
	users, err := us.Users(ctx)
	require.NoError(t, err)
	require.Len(t, users, 3)
	for _, u := range users {
		assert.True(t, auth.CheckPassword(u.Password, DemoPassword), u.Name)
	}
	admin, err := us.UserByName(ctx, "admin")
	require.NoError(t, err)
	assert.True(t, admin.Admin)
	alice, err := us.UserByName(ctx, "alice")
	require.NoError(t, err)
	assert.False(t, alice.Admin)

	// --- the policy face: the seeded world authenticates and ranks ---
	p := &auth.StaticPolicy{UserSource: ms}
	assert.Equal(t, "alice", p.Identify(basicReq(t, "alice", DemoPassword)).User)
	assert.True(t, p.Identify(basicReq(t, "alice", "wrong")).Anonymous())
	assert.True(t, p.CanAdmin(p.Identify(basicReq(t, "admin", DemoPassword)), ""))

	// --- the repository: labeled, described, member-governed ---
	info, err := ms.GetRepo(ctx, "demo")
	require.NoError(t, err)
	assert.Equal(t, "main", info.DefaultBranch)
	assert.Contains(t, info.Description, "playground")
	eff, err := ms.RepoEffectiveMembers(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, eff, 2)
	assert.Equal(t, "platform", eff[0].ViaGroup, "inherited grants")

	// --- subtrees: two directory timelines with heads of their own ---
	for _, sub := range []string{"/src", "/docs"} {
		h, _, err := svc.Views.SubtreeHead(ctx, "demo", model.AnchorPath(sub), "main")
		require.NoError(t, err, sub)
		assert.NotEmpty(t, h, "%s timeline must have a head", sub)
	}

	// --- the merge-request trio: open+clean (approved), open+conflicting, merged ---
	mergeRequests, err := svc.ListMergeRequests(ctx, "demo", "")
	require.NoError(t, err)
	require.Len(t, mergeRequests, 3)
	var clean, conflicting, merged int
	for _, m := range mergeRequests {
		switch m.State {
		case meta.MergeRequestStateMerged:
			merged++
			assert.Equal(t, "admin", m.MergedBy)
			assert.NotEmpty(t, m.MergeCommit)
		case meta.MergeRequestStateOpen:
			// mergeability is a FIELD, not an error: PreviewMerge answers
			// conflicts inside the outcome (docs/design/merge-requests.md).
			detail, derr := svc.MergeRequestDetail(ctx, "demo", m.IID)
			require.NoError(t, derr)
			if len(detail.Conflicts) > 0 {
				conflicting++
				assert.Equal(t, "docs/notes.md", detail.Conflicts[0])
			} else {
				clean++
				assert.True(t, detail.Mergeable)
				assert.NotEmpty(t, detail.Approvals, "the clean request carries bob's approval")
			}
		}
	}
	assert.Equal(t, 1, clean)
	assert.Equal(t, 1, conflicting)
	assert.Equal(t, 1, merged)

	// --- the release rule and the branches ---
	rules, err := ms.BranchRules(ctx, "demo")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "release", rules[0].Branch)
	assert.Equal(t, meta.RuleMergeRequestOnly, rules[0].Level)
	refs, err := svc.ListRefs(ctx, "demo", "")
	require.NoError(t, err)
	branchNames := map[string]bool{}
	for _, r := range refs {
		branchNames[r.Name] = true
	}
	for _, b := range []string{
		"refs/heads/main", "refs/heads/release", "refs/heads/alice/wip",
		"refs/heads/hello", "refs/heads/conflict",
	} {
		assert.True(t, branchNames[b], "branch %s", b)
	}

	// --- idempotence: a re-run neither doubles nor errors ---
	require.NoError(t, Seed(ctx, svc, t.TempDir()))
	mergeRequests2, err := svc.ListMergeRequests(ctx, "demo", "")
	require.NoError(t, err)
	assert.Len(t, mergeRequests2, 3)
	users2, _ := us.Users(ctx)
	assert.Len(t, users2, 3)
}

func basicReq(t *testing.T, user, pass string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, err)
	r.SetBasicAuth(user, pass)
	return r
}
