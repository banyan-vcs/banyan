// Member-governed repositories as a decorator over any auth.Service
// (docs/gateway/authorization.md §8e, docs/design/data-model.md §16): the
// member ledger from the metadata plane (meta.MemberStore) decides who may
// download and commit on repositories it governs — a repo is governed when
// it carries ANY direct member row, or its group label's group carries ANY
// member row. Ungoverned repositories pass through to the inner policy
// unchanged, so deployments that never manage members behave exactly as
// before (the SubtreeRulePolicy philosophy, on the same seam).
//
// The ranks: viewer fetches, developer also pushes, owner manages — and the
// inner policy's admins outrank everything (they own the management plane
// that edits these tables; locking themselves out would brick it). The
// decorator only ever gates: an admitted member still passes through the
// inner policy, so scopes, protected branches and personal namespaces all
// keep applying underneath.
//
//	MemberPolicy{Inner: TokenPolicy{Inner: StaticPolicy{...}}, Members: ms}
package auth

import (
	"context"
	"net/http"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// MemberPolicy wraps an inner Service with the member ledger. All methods
// are safe for concurrent use.
type MemberPolicy struct {
	Inner   Service
	Members meta.MemberStore
}

func (p *MemberPolicy) Identify(h *http.Request) Identity {
	return p.Inner.Identify(h)
}

func (p *MemberPolicy) IdentifyKey(user string, keyBlob []byte) Identity {
	return p.Inner.IdentifyKey(user, keyBlob)
}

func (p *MemberPolicy) VisibleRefs(id Identity, repo string, refs []Ref) []Ref {
	return p.Inner.VisibleRefs(id, repo, refs)
}

// rankOn answers the identity's effective rank and whether the repo is
// member-governed at all. A ledger error fails closed (the GitLab /allowed
// posture): governed-with-no-rank, so the caller denies quietly rather than
// falling open.
func (p *MemberPolicy) rankOn(id Identity, repo string) (rank string, governed bool) {
	rank, governed, err := p.Members.MemberRank(context.Background(), id.User, repo)
	if err != nil {
		return "", true
	}
	return rank, governed
}

// passes answers whether the identity clears the member gate for the wanted
// height: viewer for download, developer for commit.
func (p *MemberPolicy) passes(id Identity, repo, want string) bool {
	rank, governed := p.rankOn(id, repo)
	if !governed {
		return true // ungoverned — the inner policy decides alone
	}
	if ac, ok := p.Inner.(AdminChecker); ok && ac.CanAdmin(id, repo) {
		return true
	}
	return meta.RankVal(rank) >= meta.RankVal(want)
}

func (p *MemberPolicy) CanFetch(id Identity, repo, sub string) bool {
	if !p.passes(id, repo, meta.RankViewer) {
		return false
	}
	return p.Inner.CanFetch(id, repo, sub)
}

func (p *MemberPolicy) CanPush(id Identity, repo, sub, branch string) bool {
	if !p.passes(id, repo, meta.RankDeveloper) {
		return false
	}
	return p.Inner.CanPush(id, repo, sub, branch)
}

// CanAdmin (auth.AdminChecker): the inner policy's admins — the member
// ledger guards git traffic, not the management plane that edits it.
func (p *MemberPolicy) CanAdmin(id Identity, repo string) bool {
	if ac, ok := p.Inner.(AdminChecker); ok {
		return ac.CanAdmin(id, repo)
	}
	return false
}
