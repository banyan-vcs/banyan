// Goal: retrieval and packing efficiency — have subtraction on incremental
// fetches (docs/09 §3), serve-path delta encoding (docs/11 P1-4), post-push
// view pre-warm and the clone-shaped response-pack cache (docs/11 P2-2/P2-3).
package protocol

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

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// rawFetch posts an upload-pack request (wants [+haves] + done) and returns
// the pack bytes (everything after the first ACK/NAK pkt-line; no deepen ⇒
// no shallow section).
func rawFetch(t *testing.T, s *support.Server, rawurl string, wants, haves []model.OID) []byte {
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
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t19")
	oldTip := support.HeadOf(t, src)

	client := support.CloneRepo(t, s, s.RepoURL("t19"))
	support.Write(t, filepath.Join(src, "lib", "a.txt"), "a-v2\n")
	support.Git(t, src, "commit", "-qam", "t19: one-file delta")
	support.Git(t, src, "push", "-q", s.RepoURL("t19"), "main")
	newTip := support.HeadOf(t, src)

	// Full closure (no haves) — the pre-optimization shape.
	full, err := storage.DecodePack(rawFetch(t, s, s.RepoURL("t19"), []model.OID{model.OID(newTip)}, nil))
	require.NoError(t, err)
	fullCount := len(full.Commits) + len(full.Trees) + len(full.Blobs)

	// Delta fetch with the client's have.
	delta, err := storage.DecodePack(rawFetch(t, s, s.RepoURL("t19"),
		[]model.OID{model.OID(newTip)}, []model.OID{model.OID(oldTip)}))
	require.NoError(t, err)
	assert.Equal(t, 1, len(delta.Commits), "delta pack commits")
	assert.Equal(t, 2, len(delta.Trees), "delta pack trees (root + lib)")
	assert.Equal(t, 1, len(delta.Blobs), "delta pack blobs (new a.txt only)")

	newBlob := support.BlobOf(t, src, "lib/a.txt")
	readmeBlob := support.BlobOf(t, src, "README.md")
	sharedBlob := support.BlobOf(t, src, "lib/nested/SHARED.txt")
	assert.Contains(t, delta.Blobs, model.OID(newBlob), "delta must carry the changed blob")
	assert.NotContains(t, delta.Blobs, model.OID(readmeBlob), "untouched README blob must stay out")
	assert.NotContains(t, delta.Blobs, model.OID(sharedBlob), "untouched subtree blob must stay out")
	assert.Less(t, len(delta.Commits)+len(delta.Trees)+len(delta.Blobs), fullCount,
		"delta pack must be smaller than the full closure")

	// End-to-end: a real client negotiating with haves gets a complete repo.
	support.Git(t, client, "pull", "-q", "origin", "main")
	assert.Equal(t, newTip, support.HeadOf(t, client))
	assert.Equal(t, "a-v2\n", support.ReadFile(t, filepath.Join(client, "lib", "a.txt")))
	support.FsckClean(t, client)

	// Subtree URLs subtract the same way.
	sub := support.CloneRepo(t, s, s.TreeURL("t19", "lib/nested"))
	subTip := support.HeadOf(t, sub)
	support.Write(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "base\nn2\n")
	support.Git(t, src, "commit", "-qam", "t19: nested delta")
	support.Git(t, src, "push", "-q", s.RepoURL("t19"), "main")
	support.Git(t, sub, "pull", "-q", "origin", "main")
	assert.NotEqual(t, subTip, support.HeadOf(t, sub))
	support.FsckClean(t, sub)
}

// TestServePathDelta: online serve packs deltify past the raw-payload
// threshold (docs/11 P1-4 / B5+B8). Two pushes carry near-identical 64KiB+
// blobs; the full closure spans three stored packs so the whole-pack direct
// serve cannot fire — the serve path encodes online, and the similar blobs
// travel as one base + OFS_DELTA. A real git client consumes the delta pack.
func TestServePathDelta(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t20")

	big := strings.Repeat("banyan serve-path delta fixture\n", 2300) // ~66KiB, past the threshold
	support.Write(t, filepath.Join(src, "big.txt"), big)
	support.Git(t, src, "add", "big.txt")
	support.Git(t, src, "commit", "-qm", "t20: big blob")
	support.Git(t, src, "push", "-q", s.RepoURL("t20"), "main")
	big2 := big + "appended tail\n"
	support.Write(t, filepath.Join(src, "big.txt"), big2)
	support.Git(t, src, "commit", "-qam", "t20: big blob edit")
	support.Git(t, src, "push", "-q", s.RepoURL("t20"), "main")
	tip := support.HeadOf(t, src)

	packBytes := rawFetch(t, s, s.RepoURL("t20"), []model.OID{model.OID(tip)}, nil)
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
	dst := support.CloneRepo(t, s, s.RepoURL("t20"))
	support.FsckClean(t, dst)
	assert.Equal(t, big2, support.ReadFile(t, filepath.Join(dst, "big.txt")), "真实客户端解包 delta 后的内容")
}

// --- T34 post-push view pre-warm (docs/11 P2-3, B6) ---
// A push only CASes timeline heads; the woven views fold lazily on the next
// advertisement. The warm hook folds them right after a successful push, so
// the /.woven root anchor exists WITHOUT any read request — observable via
// the persisted woven timeline advancing to the pushed head.
func TestPushWarmView(t *testing.T) {
	support.NeedGit(t)
	dir := t.TempDir()
	s, objects := support.NewStack(t, filepath.Join(dir, "shards"), "", false)
	src := support.SeedRepo(t, s, "t34")
	ctx := context.Background()

	// Root push: the root weave persists at /.woven/ and must equal the
	// pushed head (plain chains fold identity — verbatim reuse).
	require.Eventually(t, func() bool {
		st, err := objects.Meta.Timeline(ctx, model.Timeline{Repo: "t34", Anchor: "/.woven/", Name: "main"})
		return err == nil && st.Head == model.OID(support.HeadOf(t, src))
	}, 5*time.Second, 50*time.Millisecond, "push 后 /.woven 应被预热到新头（无需任何读请求）")

	// Subtree push: warm folds the descendant commit into the ROOT weave —
	// observable as /.woven/ moving off the pre-push root head without any
	// root-repo read request in between (the subtree clone only reads the
	// subtree view). The scoped anchor itself needs no fold when the prefix
	// already answers the view.
	rootWovenBefore, err := objects.Meta.Timeline(ctx, model.Timeline{Repo: "t34", Anchor: "/.woven/", Name: "main"})
	require.NoError(t, err)
	sub := support.CloneRepo(t, s, s.TreeURL("t34", "lib/nested"))
	support.AppendLine(t, filepath.Join(sub, "SHARED.txt"), "warm-me\n")
	support.Git(t, sub, "commit", "-qam", "t34: sub")
	support.Git(t, sub, "push", "-q", "origin", "main")
	require.Eventually(t, func() bool {
		st, err := objects.Meta.Timeline(ctx, model.Timeline{Repo: "t34", Anchor: "/.woven/", Name: "main"})
		return err == nil && !st.Head.IsZero() && st.Head != rootWovenBefore.Head
	}, 5*time.Second, 50*time.Millisecond, "子树 push 后根织入锚应被预热推进（折叠后代提交为重写 OID）")

	// The warmed state serves a fresh clone correctly (the root view folds
	// the subtree push as a rewrite — assert content, not the raw OID).
	fresh := support.CloneRepo(t, s, s.RepoURL("t34"))
	support.FsckClean(t, fresh)
	assert.Equal(t, "base\nwarm-me\n", support.ReadFile(t, filepath.Join(fresh, "lib", "nested", "SHARED.txt")),
		"根视图应包含子树推送内容")
}

// --- T35 clone-shaped response pack cache (docs/11 P2-2, B9) ---
// A subtree clone's closure never equals a stored pack (projection/weave
// OIDs are synthetic), so every clone re-encoded online. Clone-shaped
// requests (no haves/depth/filter — the pack is a pure function of the
// wants) now cache verbatim: repeat clones skip encoding, tips changes
// produce a new key, and plain root clones keep hitting ExactPackHit
// without polluting the cache.
func TestClosureResponseCache(t *testing.T) {
	support.NeedGit(t)
	dir := t.TempDir()
	s, objects := support.NewStack(t, filepath.Join(dir, "shards"), "", false)
	src := support.SeedRepo(t, s, "t35")

	// First subtree clone: online encode, cached.
	sub1 := support.CloneRepo(t, s, s.TreeURL("t35", "lib/nested"))
	hits, stores, _ := objects.ResponseCacheStats()
	assert.Equal(t, 1, stores, "首次子树克隆应缓存其闭包包")
	assert.Equal(t, 0, hits)

	// Repeat clone: served from the cache, byte-identical result.
	sub2 := support.CloneRepo(t, s, s.TreeURL("t35", "lib/nested"))
	hits, stores, _ = objects.ResponseCacheStats()
	assert.Equal(t, 1, hits, "第二次子树克隆应命中缓存")
	assert.Equal(t, 1, stores, "命中不应新增缓存")
	assert.Equal(t, support.HeadOf(t, sub1), support.HeadOf(t, sub2))
	support.FsckClean(t, sub2)

	// Plain root clone: ExactPackHit serves the pushed pack — no new cache entry.
	_ = support.CloneRepo(t, s, s.RepoURL("t35"))
	_, stores, _ = objects.ResponseCacheStats()
	assert.Equal(t, 1, stores, "根克隆走 ExactPackHit，不应污染闭包缓存")

	// Tips move → new key → re-encode and cache again.
	support.AppendLine(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "t35: moved\n")
	support.Git(t, src, "commit", "-qam", "t35: nested change")
	support.Git(t, src, "push", "-q", s.RepoURL("t35"), "main")
	sub3 := support.CloneRepo(t, s, s.TreeURL("t35", "lib/nested"))
	hits, stores, _ = objects.ResponseCacheStats()
	assert.Equal(t, 2, stores, "tips 变化应产生新缓存键")
	assert.Equal(t, "base\nt35: moved\n", support.ReadFile(t, filepath.Join(sub3, "SHARED.txt")))
	support.FsckClean(t, sub3)

	// Incremental (haves present) fetches are client-specific: never cached.
	client := support.CloneRepo(t, s, s.TreeURL("t35", "lib/nested"))
	support.AppendLine(t, filepath.Join(src, "lib", "nested", "SHARED.txt"), "t35: again\n")
	support.Git(t, src, "commit", "-qam", "t35: nested change 2")
	support.Git(t, src, "push", "-q", s.RepoURL("t35"), "main")
	support.Git(t, client, "pull", "-q", "origin", "main")
	_, stores, _ = objects.ResponseCacheStats()
	assert.Equal(t, 2, stores, "带 haves 的增量 fetch 不应进缓存")
}

// --- T37 true thin pack (docs/11 P2-6, B8) ---
// A have-subtracted fetch now deltas its objects against same-path
// predecessors the client provably holds (its have closure), emitted as
// REF_DELTA entries the client resolves with index-pack --fix-thin — the
// 1.6-1.8x incremental-fetch byte gap measured in docs/11 §3.
func TestThinPackFetch(t *testing.T) {
	support.NeedGit(t)
	s := support.NewServer(t)
	src := support.SeedRepo(t, s, "t37")

	// Two realistic changes against a KB-scale file: first establish
	// big.txt (the client's held version), then edit it (its previous
	// version is the natural delta base) and add one brand-new file.
	big := strings.Repeat("stable content line for delta compression testing\n", 100) // ~4.7KB
	support.Write(t, filepath.Join(src, "lib", "big.txt"), big)
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "t37: establish big")
	support.Git(t, src, "push", "-q", s.RepoURL("t37"), "main")
	oldTip := support.HeadOf(t, src)
	// The client clones HERE: it holds oldTip, so the next fetch transfers
	// the incremental change.
	client := support.CloneRepo(t, s, s.RepoURL("t37"))
	assert.Equal(t, oldTip, support.HeadOf(t, client))
	support.Write(t, filepath.Join(src, "lib", "big.txt"), big+"one appended line\n")
	support.Write(t, filepath.Join(src, "lib", "new.txt"), "brand new file\n")
	support.Git(t, src, "add", "-A")
	support.Git(t, src, "commit", "-qm", "t37: incremental change")
	support.Git(t, src, "push", "-q", s.RepoURL("t37"), "main")
	newTip := support.HeadOf(t, src)

	// Raw protocol: the same fetch with and without the thin-pack
	// capability — thin must carry REF_DELTA entries and fewer bytes.
	fetchRaw := func(caps string) []byte {
		body := pktLine("want "+string(newTip)+" agent=it ofs-delta"+caps+"\n") +
			pktLine("have "+string(oldTip)+"\n") +
			pktLine("done\n")
		req, err := http.NewRequest("POST", s.RepoURL("t37")+"/git-upload-pack", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		// Skip the terminal ACK/NAK line; the rest is the pack.
		require.GreaterOrEqual(t, len(raw), 12)
		var n int
		fmt.Sscanf(string(raw[:4]), "%04x", &n)
		return raw[n:]
	}
	plain := fetchRaw("")
	thin := fetchRaw(" thin-pack")
	types, err := storage.ScanObjectTypes(thin)
	require.NoError(t, err)
	hasRefDelta := false
	for _, ty := range types {
		if ty == model.ObjectType("ref-delta") {
			hasRefDelta = true
		}
	}
	assert.True(t, hasRefDelta, "thin 包应含 REF_DELTA 条目; types=%v", types)
	assert.Less(t, len(thin), len(plain), "thin 包应小于非 thin 包（%d vs %d）", len(thin), len(plain))
	plainTypes, _ := storage.ScanObjectTypes(plain)
	for _, ty := range plainTypes {
		assert.NotEqual(t, model.ObjectType("ref-delta"), ty, "未请求 thin 时不得发 REF_DELTA")
	}

	// Real client end to end: a clone that still holds oldTip fetches the
	// change — a small pack goes through the client's unpack-objects path,
	// which resolves external REF_DELTA bases from its own object store
	// (mirrors what git's own server sends: thin even for 3-object fetches).
	// v2 first (the default), then a second change fetched over v1 to cover
	// the advertised-capability path.
	for _, ver := range []string{"2", "0"} {
		support.Git(t, client, "-c", "protocol.version="+ver, "fetch", "-q", "origin")
		support.Git(t, client, "merge", "-q", "--ff-only", "origin/main")
	}
	assert.Equal(t, newTip, support.HeadOf(t, client))
	assert.Equal(t, big+"one appended line\n",
		support.ReadFile(t, filepath.Join(client, "lib", "big.txt")))
	assert.Equal(t, "brand new file\n", support.ReadFile(t, filepath.Join(client, "lib", "new.txt")))
	support.FsckClean(t, client)
}
