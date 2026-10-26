package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testSnapshot() *UpstreamSnapshot {
	return &UpstreamSnapshot{
		Version: 2,
		Admins:  []string{"root"},
		Users:   []string{"root", "alice", "bob"},
		Repos: map[string]UpstreamRepo{
			"p_x": {
				ProtectedBranches: []string{"main"},
				Anchors: map[string]UpstreamAnchor{
					"":             {Members: map[string]string{"alice": "owner", "bob": "developer"}},
					"services/api": {Members: map[string]string{"carol": "maintainer"}},
				},
			},
			"p_pub": {
				Public: true,
				Anchors: map[string]UpstreamAnchor{
					"": {Members: map[string]string{}},
				},
			},
		},
	}
}

func newTestUpstream(t *testing.T, snap *UpstreamSnapshot) *UpstreamPolicy {
	t.Helper()
	p := NewUpstreamPolicy("http://unused", []byte("k-test"))
	p.snap = snap
	p.at = time.Now()
	return p
}

func idOf(user string) Identity { return Identity{User: user, Name: user} }

// Identity header roundtrip plus tamper/replay resistance.
func TestSignedIdentity(t *testing.T) {
	secret := []byte("k-test")
	now := time.Now()
	v := SignIdentity(secret, "alice", now)
	user, ok := ParseIdentity(secret, v, now)
	require.True(t, ok)
	require.Equal(t, "alice", user)

	_, ok = ParseIdentity([]byte("wrong"), v, now)
	require.False(t, ok)
	_, ok = ParseIdentity(secret, v+"00", now)
	require.False(t, ok)
	_, ok = ParseIdentity(secret, "alice."+now.Format("150405")+".deadbeef", now)
	require.False(t, ok)
	// Outside the ±30s window: stale and future both fail.
	_, ok = ParseIdentity(secret, SignIdentity(secret, "alice", now.Add(-2*time.Minute)), now)
	require.False(t, ok)
	_, ok = ParseIdentity(secret, SignIdentity(secret, "alice", now.Add(2*time.Minute)), now)
	require.False(t, ok)
}

// The gateway-side aggregation mirrors the console's max-over-prefix-chain.
func TestUpstreamPolicyDecisions(t *testing.T) {
	p := newTestUpstream(t, testSnapshot())

	// Fetch: member yes, stranger no, public anonymous-equivalent yes.
	require.True(t, p.CanFetch(idOf("bob"), "p_x", ""))
	require.True(t, p.CanFetch(idOf("bob"), "p_x", "services/api"))
	require.False(t, p.CanFetch(idOf("stranger"), "p_x", ""))
	require.True(t, p.CanFetch(Identity{}, "p_pub", ""))
	// Carol holds maintainer ONLY at the anchor: root fetch is denied, the
	// anchor itself works (granting at depth is the enforceable direction).
	require.False(t, p.CanFetch(idOf("carol"), "p_x", ""))
	require.True(t, p.CanFetch(idOf("carol"), "p_x", "services/api"))

	// Push: developer on ordinary branches, protected main needs owner.
	require.True(t, p.CanPush(idOf("bob"), "p_x", "", "feature"))
	require.False(t, p.CanPush(idOf("bob"), "p_x", "", "main"))
	require.True(t, p.CanPush(idOf("alice"), "p_x", "", "main"))
	// Admin sees and does everything (docs/07 §3).
	require.True(t, p.CanPush(idOf("root"), "p_x", "", "main"))

	// Personal branches: owner or admin only.
	require.True(t, p.CanPush(idOf("bob"), "p_x", "", "bob/wip"))
	require.False(t, p.CanPush(idOf("alice"), "p_x", "", "bob/wip"))
	require.True(t, p.CanPush(idOf("root"), "p_x", "", "bob/wip"))

	// Unknown repos deny (with a force-refresh attempt against a dead URL:
	// fail closed, fast).
	require.False(t, p.CanFetch(idOf("alice"), "p_missing", ""))

	// VisibleRefs: a personal branch hides from strangers, not its owner.
	refs := []Ref{{Name: "refs/heads/main"}, {Name: "refs/heads/alice/wip"}}
	vis := p.VisibleRefs(idOf("bob"), "p_x", refs)
	require.Len(t, vis, 1)
	vis = p.VisibleRefs(idOf("alice"), "p_x", refs)
	require.Len(t, vis, 2)
}

// Identify reads only the signed header — Basic credentials have no effect
// in this topology.
func TestUpstreamIdentify(t *testing.T) {
	p := newTestUpstream(t, testSnapshot())
	h, _ := http.NewRequest("GET", "/x", nil)
	require.True(t, p.Identify(h).Anonymous())
	h.Header.Set("Authorization", "Basic YWxpY2U6cGFzcw==")
	require.True(t, p.Identify(h).Anonymous())
	h.Header.Set(IdentityHeaderName, SignIdentity(p.Secret, "alice", time.Now()))
	require.Equal(t, "alice", p.Identify(h).User)
}
