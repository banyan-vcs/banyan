package auth

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// RemotePolicy serves the gateway role (docs/13 §4): decisions run against
// a policy snapshot synced from the refs service — the cluster's policy
// source of truth — and cached briefly. The gateway never hand-rolls rules;
// it runs the same abstraction with synced state, so demo and cluster
// enforce identically. On sync failure the last snapshot stays in effect;
// with no snapshot every decision fails CLOSED (anonymous/denied).
type RemotePolicy struct {
	RefsURL string // refs service base URL (http://host:port)
	HTTP    *http.Client

	mu   sync.Mutex
	snap *StaticPolicy
	at   time.Time
}

const policySyncTTL = 10 * time.Second

func NewRemotePolicy(refsURL string) *RemotePolicy {
	return &RemotePolicy{RefsURL: refsURL, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func (p *RemotePolicy) snapshot() *StaticPolicy {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.snap != nil && time.Since(p.at) < policySyncTTL {
		return p.snap
	}
	resp, err := p.HTTP.Get(p.RefsURL + "/internal/authz/policy")
	if err != nil {
		return p.snap // keep the last known policy; nil = fail closed
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p.snap
	}
	var snap StaticPolicy
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return p.snap
	}
	p.snap, p.at = &snap, time.Now()
	return p.snap
}

func (p *RemotePolicy) Identify(h *http.Request) Identity {
	if snap := p.snapshot(); snap != nil {
		return snap.Identify(h)
	}
	return Identity{} // no policy synced: anonymous (callers challenge)
}

func (p *RemotePolicy) VisibleRefs(id Identity, repo string, refs []Ref) []Ref {
	if snap := p.snapshot(); snap != nil {
		return snap.VisibleRefs(id, repo, refs)
	}
	return nil
}

func (p *RemotePolicy) CanFetch(id Identity, repo, sub string) bool {
	return p.snapshot() != nil && p.snapshot().CanFetch(id, repo, sub)
}

func (p *RemotePolicy) CanPush(id Identity, repo, sub, branch string) bool {
	return p.snapshot() != nil && p.snapshot().CanPush(id, repo, sub, branch)
}
