package storage

import (
	"context"
	"testing"

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
