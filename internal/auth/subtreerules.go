// Runtime subtree access rules as a decorator over any auth.Service
// (docs/gateway/authorization.md §8): rules from the metadata plane (meta.SubtreeRuleStore)
// narrow what the inner policy allows, at directory granularity, for both
// download and commit. The rule at the SHALLOWEST anchor covering the
// requested subtree governs — a rule on /src also protects /src/lib, because
// a subtree view's closure contains everything deeper (docs/web/console.md: "to hide a
// subpath, tighten at the shallowest anchor covering it"). The reverse is
// impossible by design and stays a documented boundary: a rule on /src/lib
// cannot hide /src, since fetching the shallower anchor serves the deeper
// content as part of its closure.
//
// Paths without a covering rule delegate to the inner policy unchanged —
// deployments that never manage rules behave exactly as before. This is the
// extensibility seam in action, the TokenPolicy composition pattern:
//
//	SubtreeRulePolicy{Inner: GitLabSSO{...}, Rules: meta}
//	SubtreeRulePolicy{Inner: TokenPolicy{Inner: StaticPolicy{...}}, Rules: meta}
//
// Rules only ever NARROW: a listed user still passes through the inner
// policy (scopes, protected branches, personal namespaces all keep applying).
package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/banyan-vcs/banyan/internal/meta"
)

// SubtreeRulePolicy wraps an inner Service with metadata-plane directory
// access rules. All methods are safe for concurrent use.
type SubtreeRulePolicy struct {
	Inner Service
	Rules meta.SubtreeRuleStore
}

func (p *SubtreeRulePolicy) Identify(h *http.Request) Identity {
	return p.Inner.Identify(h)
}

func (p *SubtreeRulePolicy) IdentifyKey(user string, keyBlob []byte) Identity {
	return p.Inner.IdentifyKey(user, keyBlob)
}

func (p *SubtreeRulePolicy) VisibleRefs(id Identity, repo string, refs []Ref) []Ref {
	return p.Inner.VisibleRefs(id, repo, refs)
}

// ruleFor answers the shallowest rule covering the anchor (sub "" is the
// root repo, normalized to "/"), or nil when the path is unmanaged.
func (p *SubtreeRulePolicy) ruleFor(repo, sub string) *meta.SubtreeRule {
	if sub == "" {
		sub = "/"
	}
	rules, err := p.Rules.SubtreeRules(context.Background(), repo)
	if err != nil {
		// The rule plane is unavailable: deny quietly rather than fall open
		// (fail-closed, the same posture as the GitLab /allowed gate).
		return &meta.SubtreeRule{Path: sub, Users: []string{}}
	}
	var found *meta.SubtreeRule
	for i := range rules {
		path := rules[i].Path
		covers := path == "/" || sub == path || strings.HasPrefix(sub, path+"/")
		if !covers {
			continue
		}
		if found == nil || len(path) < len(found.Path) {
			r := rules[i]
			found = &r
		}
	}
	return found
}

// listed reports whether the identity passes a covering rule: named in the
// allowlist, or an admin of the inner policy (admins manage the rules, so
// locking themselves out would brick the management plane).
func (p *SubtreeRulePolicy) listed(id Identity, repo string, rule *meta.SubtreeRule) bool {
	for _, u := range rule.Users {
		if u == id.User {
			return true
		}
	}
	if ac, ok := p.Inner.(AdminChecker); ok {
		return ac.CanAdmin(id, repo)
	}
	return false
}

func (p *SubtreeRulePolicy) CanFetch(id Identity, repo, sub string) bool {
	if rule := p.ruleFor(repo, sub); rule != nil {
		if !p.listed(id, repo, rule) {
			return false
		}
	}
	return p.Inner.CanFetch(id, repo, sub)
}

func (p *SubtreeRulePolicy) CanPush(id Identity, repo, sub, branch string) bool {
	if rule := p.ruleFor(repo, sub); rule != nil {
		if !p.listed(id, repo, rule) {
			return false
		}
	}
	return p.Inner.CanPush(id, repo, sub, branch)
}

// CanAdmin (auth.AdminChecker): the inner policy's admins — rules guard git
// traffic, not the management plane that edits them.
func (p *SubtreeRulePolicy) CanAdmin(id Identity, repo string) bool {
	if ac, ok := p.Inner.(AdminChecker); ok {
		return ac.CanAdmin(id, repo)
	}
	return false
}
