// Goal: retrieval and packing efficiency — have subtraction on incremental
// fetches (docs/09 §3), serve-path delta encoding (docs/12 P1-4), post-push
// view pre-warm and the clone-shaped response-pack cache (docs/12 P2-2/P2-3).
package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			line += " agent=it ofs-delta" // capability section trails the first want, as real clients send it
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

// TestServePathDelta: online serve packs deltify past the raw-payload
// threshold (docs/12 P1-4 / B5+B8). Two pushes carry near-identical 64KiB+
// blobs; the full closure spans three stored packs so the whole-pack direct
// serve cannot fire — the serve path encodes online, and the similar blobs
// travel as one base + OFS_DELTA. A real git client consumes the delta pack.
func TestServePathDelta(t *testing.T) {
	needGit(t)
	s := newServer(t)
	src := seedRepo(t, s, "t20")

	big := strings.Repeat("banyan serve-path delta fixture\n", 2300) // ~66KiB, past the threshold
	write(t, filepath.Join(src, "big.txt"), big)
	git(t, src, "add", "big.txt")
	git(t, src, "commit", "-qm", "t20: big blob")
	git(t, src, "push", "-q", s.repoURL("t20"), "main")
	big2 := big + "appended tail\n"
	write(t, filepath.Join(src, "big.txt"), big2)
	git(t, src, "commit", "-qam", "t20: big blob edit")
	git(t, src, "push", "-q", s.repoURL("t20"), "main")
	tip := headOf(t, src)

	packBytes := rawFetch(t, s, s.repoURL("t20"), []model.OID{model.OID(tip)}, nil)
	types, err := storage.ScanObjectTypes(packBytes)
	require.NoError(t, err)
	hasOfs := false
	for _, ty := range types {
		if ty == model.ObjectType("ofs-delta") {
			hasOfs = true
		}
	}
	assert.True(t, hasOfs, "serve pack 应含 OFS_DELTA；types=%v", types)
	assert.Less(t, len(packBytes), (len(big)+len(big2))*3/4,
		"delta 化后字节数应显著低于原始负载")

	// End-to-end: a real client receives and unpacks the online delta pack.
	dst := cloneRepo(t, s, s.repoURL("t20"))
	fsckClean(t, dst)
	assert.Equal(t, big2, readFile(t, filepath.Join(dst, "big.txt")), "真实客户端解包 delta 后的内容")
}

// --- T34 post-push view pre-warm (docs/12 P2-3, B6) ---
// A push only CASes timeline heads; the woven views fold lazily on the next
// advertisement. The warm hook folds them right after a successful push, so
// the /.woven root anchor exists WITHOUT any read request — observable via
// the persisted woven timeline advancing to the pushed head.
func TestPushWarmView(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	s, objects := newStack(t, filepath.Join(dir, "shards"), "", false)
	src := seedRepo(t, s, "t34")
	ctx := context.Background()

	// Root push: the root weave persists at /.woven/ and must equal the
	// pushed head (plain chains fold identity — verbatim reuse).
	require.Eventually(t, func() bool {
		st, err := objects.Meta.Timeline(ctx, model.Timeline{Repo: "t34", Anchor: "/.woven/", Name: "main"})
		return err == nil && st.Head == model.OID(headOf(t, src))
	}, 5*time.Second, 50*time.Millisecond, "push 后 /.woven 应被预热到新头（无需任何读请求）")

	// Subtree push: warm folds the descendant commit into the ROOT weave —
	// observable as /.woven/ moving off the pre-push root head without any
	// root-repo read request in between (the subtree clone only reads the
	// subtree view). The scoped anchor itself needs no fold when the prefix
	// already answers the view.
	rootWovenBefore, err := objects.Meta.Timeline(ctx, model.Timeline{Repo: "t34", Anchor: "/.woven/", Name: "main"})
	require.NoError(t, err)
	sub := cloneRepo(t, s, s.treeURL("t34", "lib/nested"))
	appendLine(t, filepath.Join(sub, "SHARED.txt"), "warm-me\n")
	git(t, sub, "commit", "-qam", "t34: sub")
	git(t, sub, "push", "-q", "origin", "main")
	require.Eventually(t, func() bool {
		st, err := objects.Meta.Timeline(ctx, model.Timeline{Repo: "t34", Anchor: "/.woven/", Name: "main"})
		return err == nil && !st.Head.IsZero() && st.Head != rootWovenBefore.Head
	}, 5*time.Second, 50*time.Millisecond, "子树 push 后根织入锚应被预热推进（折叠后代提交为重写 OID）")

	// The warmed state serves a fresh clone correctly (the root view folds
	// the subtree push as a rewrite — assert content, not the raw OID).
	fresh := cloneRepo(t, s, s.repoURL("t34"))
	fsckClean(t, fresh)
	assert.Equal(t, "base\nwarm-me\n", readFile(t, filepath.Join(fresh, "lib", "nested", "SHARED.txt")),
		"根视图应包含子树推送内容")
}
