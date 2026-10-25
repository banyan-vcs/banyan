// Package auth is the authorization abstraction (docs/13, docs/06 M5):
// the git surface needs three granularities — repository, directory (a
// subtree virtual repo) and branch — for the two operations git has,
// download (fetch/clone) and commit (push). Implementations decide; the
// wire layers only ever enforce through this interface, so the all-in-one
// demo can carry an in-process static policy while a gateway delegates to
// a synced policy snapshot from the refs service. A nil Service means
// authorization is off (the pre-M5 behavior every test relies on).
package auth

import (
	"net/http"
)

// Identity is the authenticated caller. User "" is anonymous.
type Identity struct {
	User string
	Name string
}

func (i Identity) Anonymous() bool { return i.User == "" }

// Ref is one advertised ref, in the wire layer's shape.
type Ref struct {
	Name string // full ref name, e.g. refs/heads/main
	OID  string
}

// Service decides access. All methods must be safe for concurrent use.
type Service interface {
	// Identify maps request credentials to an identity. The demo carries
	// HTTP Basic against a static user table; production deployments
	// substitute token introspection or a corporate SSO at this one seam.
	Identify(h *http.Request) Identity

	// VisibleRefs filters a repo's advertised refs for the identity —
	// branch visibility. The personal-branch rule lives here:
	// refs/heads/<user>/* is visible only to its owner and the admins, so
	// one user's working branches never flood anyone else's ref list (the
	// monorepo too-many-branches problem, docs/13 §3).
	VisibleRefs(id Identity, repo string, refs []Ref) []Ref

	// CanFetch decides download access at repository and directory
	// granularity: sub "" is the root repo, "/lib" the subtree virtual
	// repo at that anchor.
	CanFetch(id Identity, repo, sub string) bool

	// CanPush decides commit access for one branch update. branch is the
	// refname minus refs/heads/ ("" for auxiliary refs such as tags); sub
	// is the subtree anchor for virtual-repo pushes.
	CanPush(id Identity, repo, sub, branch string) bool
}

// PersonalOwner returns the owning user of a personal branch name
// ("<user>/<anything>" where <user> matches a known user), or "".
type ownerFunc func(branch string) string
