package gitwire

import (
	"testing"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

func hook(id, dir string, events ...string) storage.HookInfo {
	return storage.HookInfo{ID: id, Dir: dir, Events: events, URL: "http://hook.example/" + id}
}

func TestMatchHooks(t *testing.T) {
	hooks := []storage.HookInfo{
		hook("root", "", "pre-receive", "update"),
		hook("lib", "lib", "pre-receive"),
		hook("nested", "lib/nested", "pre-receive", "post-receive"),
		hook("doc", "doc", "update"),
	}

	ids := func(dir, event string) []string {
		var out []string
		for _, h := range matchHooks(hooks, dir, event) {
			out = append(out, h.ID)
		}
		return out
	}

	if got := ids("lib/nested", "pre-receive"); len(got) != 3 || got[0] != "nested" || got[1] != "lib" || got[2] != "root" {
		t.Fatalf("lib/nested pre-receive: most-specific-first all-ancestors, got %v", got)
	}
	if got := ids("lib", "pre-receive"); len(got) != 2 || got[0] != "lib" {
		t.Fatalf("lib pre-receive: lib then root, got %v", got)
	}
	if got := ids("", "pre-receive"); len(got) != 1 || got[0] != "root" {
		t.Fatalf("root push matches repo-level hooks only, got %v", got)
	}
	if got := ids("lib", "update"); len(got) != 1 || got[0] != "root" {
		t.Fatalf("update on lib: only the repo-level update hook, got %v", got)
	}
	if got := ids("lib/nested", "post-receive"); len(got) != 1 || got[0] != "nested" {
		t.Fatalf("post-receive on lib/nested: nested only, got %v", got)
	}
	if got := ids("other", "pre-receive"); len(got) != 1 || got[0] != "root" {
		t.Fatalf("unrelated directory falls to repo level, got %v", got)
	}
	if got := ids("lib", "post-receive"); len(got) != 0 {
		t.Fatalf("no matching post-receive, got %v", got)
	}
}

func TestHookOIDZeroRendering(t *testing.T) {
	if got := hookOID("", model.SHA1); got != "0000000000000000000000000000000000000000" {
		t.Fatalf("sha1 zero OID: %q", got)
	}
	if got := hookOID("abc", model.SHA1); got != "abc" {
		t.Fatalf("non-zero OID passes through: %q", got)
	}
}
