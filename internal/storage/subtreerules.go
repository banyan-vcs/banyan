// Runtime subtree access rules in the metadata plane (docs/07 §8): per-repo
// per-directory allowlists managed through the management API, unlike the
// YAML auth.subtree_access seed which is boot-time only. SubtreeRuleStore is
// deliberately NOT part of the MetaStore composite (the TokenRegistry
// precedent): only stores that own metadata implement it, and consumers
// type-assert (meta.(SubtreeRuleStore)) so remote-meta gateways compile
// unchanged.
package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// SubtreeRule is one directory-granular access rule: fetch AND push of the
// subtree at Path (and everything deeper) are restricted to Users. Path is
// an anchor path — "/" for the whole repository, "/a/b" for a subtree.
type SubtreeRule struct {
	Path  string   `json:"path"`
	Users []string `json:"users"`
}

// ValidSubtreePath reports whether p is a well-formed anchor path: "/" or
// "/seg[/seg…]" with clean, non-empty segments.
func ValidSubtreePath(p string) bool {
	if p == "/" {
		return true
	}
	if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || len(p) < 2 {
		return false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func (r SubtreeRule) Valid() error {
	if !ValidSubtreePath(r.Path) {
		return fmt.Errorf("subtree rule path must be \"/\" or \"/a/b\" (clean anchor path): %q", r.Path)
	}
	if len(r.Users) == 0 {
		return fmt.Errorf("subtree rule needs at least one allowed user (delete the rule to open the path)")
	}
	seen := map[string]bool{}
	for _, u := range r.Users {
		if u == "" || seen[u] {
			return fmt.Errorf("subtree rule users must be non-empty and unique")
		}
		seen[u] = true
	}
	return nil
}

// SubtreeRuleStore manages runtime subtree access rules. All methods are
// safe for concurrent use; DeleteSubtreeRule is idempotent (missing rows are
// not an error). Set on an unknown repo fails with ErrRepoNotFound.
type SubtreeRuleStore interface {
	// SubtreeRules answers the repo's rules, path-sorted.
	SubtreeRules(ctx context.Context, repo string) ([]SubtreeRule, error)
	// SetSubtreeRule upserts one rule by path.
	SetSubtreeRule(ctx context.Context, repo string, rule SubtreeRule) error
	// DeleteSubtreeRule removes one rule (missing = no-op) — the path falls
	// back to the inner policy's defaults.
	DeleteSubtreeRule(ctx context.Context, repo, path string) error
}

// --- memory implementation ---

func (m *MemoryMetaStore) SubtreeRules(_ context.Context, repo string) ([]SubtreeRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SubtreeRule
	for path, users := range m.subtreeRules[repo] {
		out = append(out, SubtreeRule{Path: path, Users: append([]string(nil), users...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (m *MemoryMetaStore) SetSubtreeRule(_ context.Context, repo string, rule SubtreeRule) error {
	if err := rule.Valid(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
	if _, ok := m.repos[repo]; !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	sort.Strings(rule.Users)
	if m.subtreeRules[repo] == nil {
		m.subtreeRules[repo] = map[string][]string{}
	}
	m.subtreeRules[repo][rule.Path] = rule.Users
	return nil
}

func (m *MemoryMetaStore) DeleteSubtreeRule(_ context.Context, repo, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenceHit(repo) {
		return fenceErr(repo)
	}
	if m.subtreeRules[repo] != nil {
		delete(m.subtreeRules[repo], path)
	}
	return nil
}

// --- SQL implementation (sqlite / mysql / postgres share it) ---

func (s *SQLMetaStore) SubtreeRules(ctx context.Context, repo string) ([]SubtreeRule, error) {
	rows, err := s.db.QueryContext(ctx, s.q(
		"SELECT path, users FROM subtree_rules WHERE repo=? ORDER BY path"), repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubtreeRule
	for rows.Next() {
		var r SubtreeRule
		var users string
		if err := rows.Scan(&r.Path, &users); err != nil {
			return nil, err
		}
		r.Users = strings.Split(users, ",")
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLMetaStore) SetSubtreeRule(ctx context.Context, repo string, rule SubtreeRule) error {
	if err := rule.Valid(); err != nil {
		return err
	}
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM repos WHERE name=?"), repo).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: %s", ErrRepoNotFound, repo)
	}
	sort.Strings(rule.Users)
	tail := "(repo, path, users) VALUES(?,?,?)"
	var stmt string
	switch s.d.name {
	case "sqlite":
		stmt = "INSERT OR REPLACE INTO subtree_rules" + tail
	case "mysql":
		stmt = "REPLACE INTO subtree_rules" + tail
	default:
		stmt = "INSERT INTO subtree_rules" + tail +
			" ON CONFLICT(repo, path) DO UPDATE SET users=excluded.users"
	}
	_, err := s.db.ExecContext(ctx, s.q(stmt), repo, rule.Path, strings.Join(rule.Users, ","))
	return err
}

func (s *SQLMetaStore) DeleteSubtreeRule(ctx context.Context, repo, path string) error {
	if err := s.fenceReject(ctx, repo); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, s.q("DELETE FROM subtree_rules WHERE repo=? AND path=?"), repo, path)
	return err
}
