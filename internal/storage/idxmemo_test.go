package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The per-object read path must decode a pack's idx ONCE per pack, not once
// per read. The real-sample bench caught the latter (docs/operations/performance.md
// §3b): a 4K-commit repository's view materialization re-decoded a 780KB
// idx tens of thousands of times and the cold clone never finished.
// Behavioral proof of the memo: after one read the idx FILE can vanish and
// later reads still resolve — a re-decoding implementation fails on the
// missing file, the memo serves from memory.
func TestIdxMemoServesWithoutIdxFile(t *testing.T) {
	ctx := context.Background()
	blobs := map[model.OID][]byte{}
	for _, s := range []string{"alpha", "beta", "gamma", "delta"} {
		blobs[model.ObjectOID(model.BlobType, []byte(s), model.SHA1)] = []byte(s)
	}
	var buf testBuffer
	require.NoError(t, EncodePack(&buf, blobs, nil, nil, nil))
	raw := buf.Bytes()

	const repo = "memo"
	inner := meta.NewMemoryMetaStore()
	require.NoError(t, inner.CreateRepo(ctx, repo, model.SHA1))
	l := NewLocalShardStore(t.TempDir())
	svc := NewObjectService(inner, l, []Node{{ID: "n1", Local: true}}, model.SHA1)
	dir := t.TempDir()
	p := filepath.Join(dir, "push.pack")
	require.NoError(t, os.WriteFile(p, raw, 0o644))
	packOID, err := svc.IngestPackFile(ctx, repo, p, int64(len(raw)))
	require.NoError(t, err)

	alpha := model.ObjectOID(model.BlobType, []byte("alpha"), model.SHA1)
	beta := model.ObjectOID(model.BlobType, []byte("beta"), model.SHA1)

	// One read installs the memo (and proves the idx file path works).
	_, got, err := l.GetObject(repo, packOID, alpha, model.SHA1)
	require.NoError(t, err)
	assert.Equal(t, "alpha", string(got))

	// The idx file disappears. The memo must serve the next read; a
	// re-decoding implementation surfaces "idx of …: not found" here.
	require.NoError(t, os.Remove(l.idxPath(repo, packOID)))
	_, got, err = l.GetObject(repo, packOID, beta, model.SHA1)
	require.NoError(t, err, "idx 消失后应经 memo 命中而非重读文件")
	assert.Equal(t, "beta", string(got))

	// Control: a COLD store (fresh memo) fails on the same missing file —
	// proves the removal actually bites and the hit above was the memo.
	cold := NewLocalShardStore(l.Root)
	_, _, err = cold.GetObject(repo, packOID, alpha, model.SHA1)
	require.Error(t, err, "冷 memo 无文件应失败（对照组）")
}
