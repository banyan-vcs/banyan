// Goal: retrieval and packing efficiency — generation-number indexes (unit:
// storage TestGenerationDiamond) and have subtraction on incremental fetches:
// the pack a client receives is exactly the delta, not the wants' full
// closure (docs/09 §3, closes docs/03 §10 deviation #1).
package integration

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// rawFetch posts an upload-pack request (wants [+haves] + done) and returns
// the pack bytes (everything after the first ACK/NAK pkt-line; no deepen ⇒
// no shallow section).
func rawFetch(t *testing.T, s *server, rawurl string, wants, haves []model.OID) []byte {
	t.Helper()
	var body strings.Builder
	for i, w := range wants {
		line := "want " + string(w)
		if i == 0 {
			line += " agent=it"
		}
		body.WriteString(pktLine(line + "\n"))
	}
	for _, h := range haves {
		body.WriteString(pktLine("have " + string(h) + "\n"))
	}
	body.WriteString(pktLine("done\n"))
	req, err := http.NewRequest("POST", rawurl+"/git-upload-pack", strings.NewReader(body.String()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(resp.Body)
	require.NoError(t, rerr)
	// Skip the terminal ACK/NAK pkt-line; the rest is the bare pack.
	require.GreaterOrEqual(t, len(raw), 12, "response too short:\n%x", raw)
	hexLen := raw[:4]
	var n int
	_, err = fmt.Sscanf(string(hexLen), "%04x", &n)
	require.NoError(t, err, "first pkt header:\n%x", raw[:8])
	require.Equal(t, "PACK", string(raw[n:n+4]), "pack must follow the terminal ACK/NAK")
	return raw[n:]
}

// --- T19 have subtraction: incremental fetch delivers exactly the delta ---
//
//	git upload-pack semantics (mark-uninteresting): objects reachable from
//	the client's haves are excluded from the pack. The delta of a one-file
//	change must be {new commit, root tree, lib tree, new blob} — untouched
//	subtrees and blobs stay out. Shallow clients keep their graft respected
//	(traversal passes through excluded commits; T15/T16 pin that path).
func TestIncrementalFetchHaveSubtraction(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t19")
	oldTip := headOf(t, src)

	client := cloneRepo(t, s, s.repoURL("t19"))
	write(t, filepath.Join(src, "lib", "a.txt"), "a-v2\n")
	git(t, src, "commit", "-qam", "t19: one-file delta")
	git(t, src, "push", "-q", s.repoURL("t19"), "main")
	newTip := headOf(t, src)

	// Full closure (no haves) — the pre-optimization shape.
	full, err := storage.DecodePack(rawFetch(t, s, s.repoURL("t19"), []model.OID{model.OID(newTip)}, nil))
	require.NoError(t, err)
	fullCount := len(full.Commits) + len(full.Trees) + len(full.Blobs)

	// Delta fetch with the client's have.
	delta, err := storage.DecodePack(rawFetch(t, s, s.repoURL("t19"),
		[]model.OID{model.OID(newTip)}, []model.OID{model.OID(oldTip)}))
	require.NoError(t, err)
	assert.Equal(t, 1, len(delta.Commits), "delta pack commits")
	assert.Equal(t, 2, len(delta.Trees), "delta pack trees (root + lib)")
	assert.Equal(t, 1, len(delta.Blobs), "delta pack blobs (new a.txt only)")

	newBlob := blobOf(t, src, "lib/a.txt")
	readmeBlob := blobOf(t, src, "README.md")
	sharedBlob := blobOf(t, src, "lib/nested/SHARED.txt")
	assert.Contains(t, delta.Blobs, model.OID(newBlob), "delta must carry the changed blob")
	assert.NotContains(t, delta.Blobs, model.OID(readmeBlob), "untouched README blob must stay out")
	assert.NotContains(t, delta.Blobs, model.OID(sharedBlob), "untouched subtree blob must stay out")
	assert.Less(t, len(delta.Commits)+len(delta.Trees)+len(delta.Blobs), fullCount,
		"delta pack must be smaller than the full closure")

	// End-to-end: a real client negotiating with haves gets a complete repo.
	git(t, client, "pull", "-q", "origin", "main")
	assert.Equal(t, newTip, headOf(t, client))
	assert.Equal(t, "a-v2\n", readFile(t, filepath.Join(client, "lib", "a.txt")))
	fsckClean(t, client)

	// Subtree URLs subtract the same way.
	sub := cloneRepo(t, s, s.treeURL("t19", "lib/nested"))
	subTip := headOf(t, sub)
	write(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "base\nn2\n")
	git(t, src, "commit", "-qam", "t19: nested delta")
	git(t, src, "push", "-q", s.repoURL("t19"), "main")
	git(t, sub, "pull", "-q", "origin", "main")
	assert.NotEqual(t, subTip, headOf(t, sub))
	fsckClean(t, sub)
}
