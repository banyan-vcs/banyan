package gitwire

import (
	"testing"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
	"github.com/banyan-vcs/banyan/internal/view"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The projection-ref gate (docs/operations/performance.md §3b 发现⑤ ②):
// refs/banyan/tree/* join the plain advertisement ONLY when the face opted
// in — gitaly/GitLab mode (fsck reads every one) or an explicit
// advertise_projections config. A default standalone clone never names a
// subtree by ref, and unadvertised projections stay unmaterialized: no
// projection chains in the pack, no ~135MiB riding every git.git clone.
func TestProjectionAdvertiseGate(t *testing.T) {
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(t.Context(), "r", model.SHA1))
	svc := storage.NewObjectService(ms, storage.NewLocalShardStore(t.TempDir()), []storage.Node{{ID: "n1", Local: true}}, model.SHA1)

	// Two commits where lib/ changes — the /lib projection derives a head.
	blob1 := model.ObjectOID(model.BlobType, []byte("lib v1"), model.SHA1)
	blob2 := model.ObjectOID(model.BlobType, []byte("lib v2"), model.SHA1)
	lib1 := &model.Tree{Entries: []model.TreeEntry{{Name: "a.txt", Mode: "100644", Target: blob1}}}
	lib2 := &model.Tree{Entries: []model.TreeEntry{{Name: "a.txt", Mode: "100644", Target: blob2}}}
	libRaw1, _ := lib1.Encode()
	libRaw2, _ := lib2.Encode()
	libOID1 := model.ObjectOID(model.TreeType, libRaw1, model.SHA1)
	libOID2 := model.ObjectOID(model.TreeType, libRaw2, model.SHA1)
	root1 := &model.Tree{Entries: []model.TreeEntry{{Name: "lib", Mode: "40000", Target: libOID1, IsDir: true}, {Name: "README", Mode: "100644", Target: blob1}}}
	root2 := &model.Tree{Entries: []model.TreeEntry{{Name: "lib", Mode: "40000", Target: libOID2, IsDir: true}, {Name: "README", Mode: "100644", Target: blob1}}}
	rootRaw1, _ := root1.Encode()
	rootRaw2, _ := root2.Encode()
	rootOID1 := model.ObjectOID(model.TreeType, rootRaw1, model.SHA1)
	rootOID2 := model.ObjectOID(model.TreeType, rootRaw2, model.SHA1)
	when := time.Unix(1760000000, 0).UTC()
	ident := model.Identity{Name: "v", Email: "v@x", When: when}
	pc1 := &model.AnchoredCommit{TreeOID: rootOID1, Author: ident, Committer: ident, Message: "one\n"}
	pc2 := &model.AnchoredCommit{TreeOID: rootOID2, Parents: []model.OID{pc1.OID(model.SHA1)}, Author: ident, Committer: ident, Message: "two\n"}
	svc.BeginSynthBatch("r")
	svc.PutSynthetic("r", blob1, model.BlobType, []byte("lib v1"))
	svc.PutSynthetic("r", blob2, model.BlobType, []byte("lib v2"))
	svc.PutSynthetic("r", libOID1, model.TreeType, libRaw1)
	svc.PutSynthetic("r", libOID2, model.TreeType, libRaw2)
	svc.PutSynthetic("r", rootOID1, model.TreeType, rootRaw1)
	svc.PutSynthetic("r", rootOID2, model.TreeType, rootRaw2)
	svc.PutSynthetic("r", pc1.OID(model.SHA1), model.CommitType, pc1.Encode())
	svc.PutSynthetic("r", pc2.OID(model.SHA1), model.CommitType, pc2.Encode())
	svc.EndSynthBatch("r")
	require.NoError(t, svc.FlushSynthBatch("r"))
	tl := model.Timeline{Repo: "r", Anchor: model.RootAnchor, Name: "main"}
	require.NoError(t, ms.EnsureTimeline(t.Context(), tl, pc2.OID(model.SHA1), model.OID("")))

	srv := &Server{Meta: ms, Objects: svc, Views: view.New(ms, svc, model.SHA1), Algo: model.SHA1}
	vr := VirtualRepo{Repo: "r", Sub: model.RootAnchor}

	refs, err := srv.ResolveRefs(t.Context(), vr)
	require.NoError(t, err)
	for _, r := range refs {
		assert.NotContains(t, r.Name, "refs/banyan/tree/", "default standalone advertisement carries no projection refs")
	}

	srv.AdvertiseProjections = true
	refs, err = srv.ResolveRefs(t.Context(), vr)
	require.NoError(t, err)
	found := false
	for _, r := range refs {
		if r.Name == "refs/banyan/tree/lib" {
			found = true
			assert.False(t, r.OID.IsZero())
		}
	}
	assert.True(t, found, "opted-in faces advertise the projection ref")
}
