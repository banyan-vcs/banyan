package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Consolidate exists for the [small-synth-pack, big-push-pack] pair the
// geometric rule leaves alone: a woven repo's clone closure spans both packs
// and whole-pack direct serve dies (docs/operations/performance.md §3b
// 合笼). The regression: two packs whose union IS the repository must merge
// into one, every object must stay readable, and ExactPackHit must fire for
// the full set — that is the clone fast path coming back.
func TestConsolidateRestoresExactPackHit(t *testing.T) {
	ctx := context.Background()
	big := map[model.OID][]byte{}   // the "push pack": 200 objects
	small := map[model.OID][]byte{} // the "synth pack": 3 objects
	for i := 0; i < 200; i++ {
		oid := model.ObjectOID(model.BlobType, []byte(string(rune('a'+i%26))+string(rune('0'+i%10))+repeatN("push", 64)), model.SHA1)
		big[oid] = []byte(string(rune('a'+i%26)) + string(rune('0'+i%10)) + repeatN("push", 64))
	}
	for i := 0; i < 3; i++ {
		oid := model.ObjectOID(model.BlobType, []byte(repeatN("synth", 32)+string(rune('0'+i))), model.SHA1)
		small[oid] = []byte(repeatN("synth", 32) + string(rune('0'+i)))
	}
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(ctx, "r", model.SHA1))
	svc := NewObjectService(ms, NewLocalShardStore(t.TempDir()), []Node{{ID: "n1", Local: true}}, model.SHA1)
	var buf testBuffer
	require.NoError(t, EncodePack(&buf, big, nil, nil, nil))
	_, err := svc.IngestPack(ctx, "r", buf.Bytes())
	require.NoError(t, err)
	buf = testBuffer{}
	require.NoError(t, EncodePack(&buf, small, nil, nil, nil))
	_, err = svc.IngestPack(ctx, "r", buf.Bytes())
	require.NoError(t, err)

	all := map[model.OID]model.ObjectType{}
	for oid := range big {
		all[oid] = model.BlobType
	}
	for oid := range small {
		all[oid] = model.BlobType
	}
	_, hit := svc.ExactPackHit("r", all)
	require.False(t, hit, "two-pack state must not hit (the premise)")

	res, err := svc.Consolidate(ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, 2, res.PacksBefore)
	assert.Equal(t, 1, res.PacksAfter, "the pair the geometric rule spares merges here")

	packOID, hit := svc.ExactPackHit("r", all)
	assert.True(t, hit, "full-set clone serves verbatim again")
	assert.NotEmpty(t, packOID)
	for oid, want := range all {
		typ, raw, rerr := svc.Raw(ctx, "r", oid)
		require.NoError(t, rerr, "object survives the merge: %s", oid)
		assert.Equal(t, want, typ)
		assert.Equal(t, true, len(raw) > 0)
	}
}

// The dominant-group exception (docs/operations/performance.md §3b 发现⑤
// fix): a [small-synth, big-push] pair whose inputs exceed the group byte
// cap must still merge when the big pack dominates — tryAppendMerge's
// working set is the dominant pack itself, and the split is what kept
// git.git's clone closure spanning two packs with the superset ladder dead.
// cap=1 forces every group over the line; dominance is purely pack size.
func TestConsolidateDominantGroupOverCap(t *testing.T) {
	ctx := context.Background()
	small := map[model.OID][]byte{}
	for i := 0; i < 3; i++ {
		b := []byte(repeatN("synth", 64) + string(rune('0'+i)))
		small[model.ObjectOID(model.BlobType, b, model.SHA1)] = b
	}
	big := map[model.OID][]byte{}
	for i := 0; i < 60; i++ {
		b := []byte(string(rune('a'+i%26)) + repeatN("push", 96) + string(rune('0'+i%10)))
		big[model.ObjectOID(model.BlobType, b, model.SHA1)] = b
	}
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(ctx, "r", model.SHA1))
	svc := NewObjectService(ms, NewLocalShardStore(t.TempDir()), []Node{{ID: "n1", Local: true}}, model.SHA1)
	for _, m := range []map[model.OID][]byte{small, big} {
		var buf testBuffer
		require.NoError(t, EncodePack(&buf, m, nil, nil, nil))
		_, err := svc.IngestPack(ctx, "r", buf.Bytes())
		require.NoError(t, err)
	}
	svc.ConsolidateMaxBytes = 1 // every addition exceeds; only dominance decides
	res, err := svc.Consolidate(ctx, "r")
	require.NoError(t, err)
	assert.Equal(t, 1, res.PacksAfter, "the dominant [synth, push] pair merges past the byte cap")
	for oid := range big {
		_, _, rerr := svc.Raw(ctx, "r", oid)
		require.NoError(t, rerr, "object readable after the capped merge: %s", oid)
	}
}

// SinglePackCover: the zero-enumeration clone rung's probe — one pack
// holding every want answers that pack; a second pack or an outside want
// answers false and the ladder proceeds (发现⑤ fix).
func TestSinglePackCover(t *testing.T) {
	ctx := context.Background()
	full := map[model.OID][]byte{}
	var want model.OID
	for i := 0; i < 20; i++ {
		b := []byte(repeatN("cover", 64) + string(rune('0'+i)))
		oid := model.ObjectOID(model.BlobType, b, model.SHA1)
		full[oid] = b
		if i == 0 {
			want = oid
		}
	}
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(ctx, "r", model.SHA1))
	svc := NewObjectService(ms, NewLocalShardStore(t.TempDir()), []Node{{ID: "n1", Local: true}}, model.SHA1)
	var buf testBuffer
	require.NoError(t, EncodePack(&buf, full, nil, nil, nil))
	_, err := svc.IngestPack(ctx, "r", buf.Bytes())
	require.NoError(t, err)

	pack, ok := svc.SinglePackCover(ctx, "r", []model.OID{want})
	assert.True(t, ok, "single pack covering the want answers")
	assert.NotEmpty(t, pack)

	missing := model.OID("0123456789012345678901234567890123456789")
	_, ok = svc.SinglePackCover(ctx, "r", []model.OID{want, missing})
	assert.False(t, ok, "a want outside the pack misses")

	buf = testBuffer{}
	extra := map[model.OID][]byte{model.ObjectOID(model.BlobType, []byte("second pack"), model.SHA1): []byte("second pack")}
	require.NoError(t, EncodePack(&buf, extra, nil, nil, nil))
	_, err = svc.IngestPack(ctx, "r", buf.Bytes())
	require.NoError(t, err)
	_, ok = svc.SinglePackCover(ctx, "r", []model.OID{want})
	assert.False(t, ok, "two-pack state misses (the union is not one stream)")
}

// Consolidate quiescence: a flush arriving under a live serve hold defers
// the merge instead of contending the meta lock under the walk; the last
// serve out re-arms it (发现⑤'s diag dump caught the self-inflicted
// registerAndSwap→PutPack lock).
func TestConsolidateQuiescence(t *testing.T) {
	ctx := context.Background()
	msQ := meta.NewMemoryMetaStore()
	require.NoError(t, msQ.CreateRepo(ctx, "r", model.SHA1))
	svcQ := NewObjectService(msQ, NewLocalShardStore(t.TempDir()), []Node{{ID: "n1", Local: true}}, model.SHA1)
	mk := func(tag string, n int) {
		m := map[model.OID][]byte{}
		for i := 0; i < n; i++ {
			b := []byte(repeatN(tag, 64) + string(rune('0'+i)))
			m[model.ObjectOID(model.BlobType, b, model.SHA1)] = b
		}
		var buf testBuffer
		require.NoError(t, EncodePack(&buf, m, nil, nil, nil))
		_, err := svcQ.IngestPack(ctx, "r", buf.Bytes())
		require.NoError(t, err)
	}
	mk("aaa", 5)
	mk("bbb", 5)

	svcQ.ServeHold("r")
	svcQ.scheduleConsolidate("r")
	svcQ.consolMu.Lock()
	sched := svcQ.consolSched["r"]
	again := svcQ.consolAgain["r"]
	svcQ.consolMu.Unlock()
	assert.False(t, sched, "no merge spawns under the serve hold")
	assert.True(t, again, "the trigger is remembered")

	svcQ.ServeRelease("r")
	require.Eventually(t, func() bool {
		packs, perr := msQ.ListPacks(ctx, "r")
		return perr == nil && len(packs) == 1
	}, 10*time.Second, 50*time.Millisecond, "the deferred merge runs when the last serve exits")
}

func repeatN(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// The consolidated repo's steady state: one pack ⊇ every clone's wants —
// PackSupersetHit must find it so the serve streams verbatim instead of
// re-encoding (the extra-objects trade git clients tolerate).
func TestPackSupersetHit(t *testing.T) {
	ctx := context.Background()
	full := map[model.OID][]byte{}
	for i := 0; i < 50; i++ {
		b := []byte(repeatN("sup", 48) + string(rune('0'+i%10)))
		full[model.ObjectOID(model.BlobType, b, model.SHA1)] = b
	}
	subset := map[model.OID]model.ObjectType{}
	i := 0
	for oid := range full {
		if i%3 == 0 { // a strict subset — a clone of some refs, not all
			subset[oid] = model.BlobType
		}
		i++
	}
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(ctx, "r", model.SHA1))
	svc := NewObjectService(ms, NewLocalShardStore(t.TempDir()), []Node{{ID: "n1", Local: true}}, model.SHA1)
	var buf testBuffer
	require.NoError(t, EncodePack(&buf, full, nil, nil, nil))
	_, err := svc.IngestPack(ctx, "r", buf.Bytes())
	require.NoError(t, err)

	packOID, ok := svc.PackSupersetHit("r", subset)
	assert.True(t, ok, "subset wants hit the superset pack")
	assert.NotEmpty(t, packOID)
	_, ok = svc.ExactPackHit("r", subset)
	assert.False(t, ok, "exact equality does not (subset ≠ pack)")

	missing := map[model.OID]model.ObjectType{model.OID("0123456789012345678901234567890123456789"): model.BlobType}
	_, ok = svc.PackSupersetHit("r", missing)
	assert.False(t, ok, "an object outside the pack misses")
}

// The strongest oracle for the append-merged pack: git itself. The writer
// claims the base entry region survives a verbatim prefix copy (OFS_DELTA
// relative offsets) and the fresh header counts base + extras exactly —
// `git index-pack --strict` validates every entry, the trailer hash, and
// the declared count against the file's actual bytes.
func TestAppendMergePackValidToGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI not available")
	}
	ctx := context.Background()
	big := map[model.OID][]byte{}
	for i := 0; i < 60; i++ {
		b := []byte(repeatN("body", 97) + string(rune('0'+i%10)) + repeatN("tail", 31))
		big[model.ObjectOID(model.BlobType, b, model.SHA1)] = b
	}
	root := t.TempDir()
	ms := meta.NewMemoryMetaStore()
	require.NoError(t, ms.CreateRepo(ctx, "r", model.SHA1))
	local := NewLocalShardStore(root)
	svc := NewObjectService(ms, local, []Node{{ID: "n1", Local: true}}, model.SHA1)
	var buf testBuffer
	require.NoError(t, EncodePack(&buf, big, nil, nil, nil))
	_, err := svc.IngestPack(ctx, "r", buf.Bytes())
	require.NoError(t, err)

	// A synth batch: three projected commits persist as their own pack
	// (real OIDs — PutSynthetic content-verifies).
	svc.BeginSynthBatch("r")
	for i := 0; i < 3; i++ {
		when := time.Unix(int64(1700000000+i), 0).UTC()
		pc := &model.AnchoredCommit{
			TreeOID:   model.EmptyTreeOID(model.SHA1),
			Author:    model.Identity{Name: "s", Email: "s@x", When: when},
			Committer: model.Identity{Name: "s", Email: "s@x", When: when},
			Message:   fmt.Sprintf("synth %d\n", i),
		}
		svc.PutSynthetic("r", pc.OID(model.SHA1), model.CommitType, pc.Encode())
	}
	svc.EndSynthBatch("r")
	require.NoError(t, svc.FlushSynthBatch("r"))
	packs, perr := ms.ListPacks(ctx, "r")
	require.NoError(t, perr)
	require.Len(t, packs, 2, "push pack + synth pack before the merge")

	// Consolidate: the dominant push pack copies verbatim, synthetics append.
	res, err := svc.Consolidate(ctx, "r")
	require.NoError(t, err)
	require.Equal(t, 1, res.PacksAfter)

	// Find the merged pack file and let git validate it end to end.
	var packPath string
	require.NoError(t, filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".pack") {
			packPath = p
		}
		return nil
	}))
	require.NotEmpty(t, packPath, "merged pack file on disk")
	out, err := exec.Command("git", "index-pack", "--strict", packPath).CombinedOutput()
	require.NoError(t, err, "git index-pack: %s", out)
}
