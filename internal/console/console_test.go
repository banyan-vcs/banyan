package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "console.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seed builds: group "acme", project "app", subprojects a, a/b, x/y (the
// latter registering an unregistered-intermediate path), plus users and the
// memberships passed in. It returns the db and a resolve helper.
func seed(t *testing.T, members map[string]map[string]Role) *DB {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	for _, name := range []string{"root", "alice", "bob", "carol"} {
		_, err := db.CreateUser(ctx, name, name == "root")
		require.NoError(t, err)
	}
	g, err := db.CreateGroup(ctx, "acme")
	require.NoError(t, err)
	p, err := db.CreateProject(ctx, g.ID, "app", "p_test01")
	require.NoError(t, err)
	for _, path := range []string{"a", "a/b", "x/y"} {
		_, err := db.CreateSubproject(ctx, p.ID, path)
		require.NoError(t, err)
	}
	for path, ms := range members {
		var nodeID int64 = p.ID
		if path != "" {
			dirs, err := db.ProjectDirs(ctx, p.ID)
			require.NoError(t, err)
			for _, d := range dirs {
				if d.Segment == path {
					nodeID = d.ID
				}
			}
		}
		for user, role := range ms {
			u, err := db.UserByName(ctx, user)
			require.NoError(t, err)
			require.NoError(t, db.SetMember(ctx, nodeID, u.ID, role))
		}
	}
	return db
}

func mustUser(t *testing.T, db *DB, name string) User {
	t.Helper()
	u, err := db.UserByName(context.Background(), name)
	require.NoError(t, err)
	return u
}

// T49a: chain resolution over registered prefixes — unregistered
// intermediates contribute nothing; deeper requests inherit shallower nodes.
func TestChainResolution(t *testing.T) {
	db := seed(t, map[string]map[string]Role{})
	ctx := context.Background()

	loc, err := db.Resolve(ctx, "acme", "app", "a/b/c")
	require.NoError(t, err)
	ids, err := db.chainIDs(ctx, loc)
	require.NoError(t, err)
	require.Len(t, ids, 4) // group, project, a, a/b — c is not registered

	loc, err = db.Resolve(ctx, "acme", "app", "x/y/z")
	require.NoError(t, err)
	ids, err = db.chainIDs(ctx, loc)
	require.NoError(t, err)
	require.Len(t, ids, 3) // group, project, x/y — "x" alone was never registered

	loc, err = db.Resolve(ctx, "acme", "app", "")
	require.NoError(t, err)
	ids, err = db.chainIDs(ctx, loc)
	require.NoError(t, err)
	require.Len(t, ids, 2) // group, project

	_, err = db.Resolve(ctx, "acme", "nope", "")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.Resolve(ctx, "acme", "app", "../etc")
	require.ErrorIs(t, err, ErrInvalid)
}

// T49b: additive-max inheritance — a role granted anywhere on the chain
// applies at the deeper anchor; sibling subtrees stay unaffected.
func TestEffectiveRoleInheritance(t *testing.T) {
	db := seed(t, map[string]map[string]Role{
		"":    {"alice": RoleDeveloper},
		"a/b": {"alice": RoleMaintainer, "bob": RoleReporter},
	})
	ctx := context.Background()
	alice, bob := mustUser(t, db, "alice"), mustUser(t, db, "bob")

	// Alice: developer at root, still developer at the unregistered sibling
	// "x", and max(developer, maintainer) = maintainer at a/b.
	r, err := db.EffectiveRole(ctx, alice, mustLoc(t, db, ""))
	require.NoError(t, err)
	require.Equal(t, RoleDeveloper, r)
	r, err = db.EffectiveRole(ctx, alice, mustLoc(t, db, "x/y"))
	require.NoError(t, err)
	require.Equal(t, RoleDeveloper, r)
	r, err = db.EffectiveRole(ctx, alice, mustLoc(t, db, "a/b"))
	require.NoError(t, err)
	require.Equal(t, RoleMaintainer, r)
	// Alice does NOT inherit bob's subproject-only grant downward-or-upward,
	// and bob has nothing outside a/b.
	r, err = db.EffectiveRole(ctx, bob, mustLoc(t, db, "a/b"))
	require.NoError(t, err)
	require.Equal(t, RoleReporter, r)
	r, err = db.EffectiveRole(ctx, bob, mustLoc(t, db, ""))
	require.NoError(t, err)
	require.Equal(t, Role(""), r)
	// Admin outranks everything everywhere.
	root := mustUser(t, db, "root")
	r, err = db.EffectiveRole(ctx, root, mustLoc(t, db, "q/r/s"))
	require.NoError(t, err)
	require.Equal(t, RoleOwner, r)
}

func mustLoc(t *testing.T, db *DB, sub string) Location {
	t.Helper()
	loc, err := db.Resolve(context.Background(), "acme", "app", sub)
	require.NoError(t, err)
	return loc
}

// T49c: rename keeps the physical identity — repo_uid is what storage ever
// sees, so a project rename is one product-DB row.
func TestRenameStability(t *testing.T) {
	db := seed(t, map[string]map[string]Role{"": {"alice": RoleOwner}})
	ctx := context.Background()
	_, p, err := db.ProjectByNames(ctx, "acme", "app")
	require.NoError(t, err)
	uid := p.RepoUID

	require.NoError(t, db.RenameNode(ctx, p.ID, "platform"))
	loc, err := db.Resolve(ctx, "acme", "platform", "a/b")
	require.NoError(t, err)
	require.Equal(t, uid, loc.Project.RepoUID)

	_, err = db.Resolve(ctx, "acme", "app", "")
	require.ErrorIs(t, err, ErrNotFound)
}

// T49d: archive hides routing but keeps rows (restore is a flag away).
func TestArchive(t *testing.T) {
	db := seed(t, map[string]map[string]Role{})
	ctx := context.Background()
	_, p, err := db.ProjectByNames(ctx, "acme", "app")
	require.NoError(t, err)
	require.NoError(t, db.ArchiveProject(ctx, p.ID))
	_, err = db.Resolve(ctx, "acme", "app", "")
	require.ErrorIs(t, err, ErrNotFound)
	projects, err := db.ListProjects(ctx)
	require.NoError(t, err)
	require.Empty(t, projects)
}

// T49e: access gates — public visibility, reporter-vs-developer split,
// member-management and project-admin gates.
func TestAccessGates(t *testing.T) {
	db := seed(t, map[string]map[string]Role{
		"":    {"alice": RoleOwner, "bob": RoleDeveloper},
		"a/b": {"carol": RoleReporter},
	})
	ctx := context.Background()
	_, p, err := db.ProjectByNames(ctx, "acme", "app")
	require.NoError(t, err)
	bob, carol := mustUser(t, db, "bob"), mustUser(t, db, "carol")

	ok, err := db.CanFetch(ctx, carol, mustLoc(t, db, "a/b"))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = db.CanPush(ctx, carol, mustLoc(t, db, "a/b"))
	require.NoError(t, err)
	require.False(t, ok) // reporter
	ok, err = db.CanPush(ctx, bob, mustLoc(t, db, "x/y"))
	require.NoError(t, err)
	require.True(t, ok) // developer via root inheritance

	// Private: anonymous has nothing; public: anonymous reads everything.
	anon := User{}
	ok, err = db.CanFetch(ctx, anon, mustLoc(t, db, ""))
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, db.SetVisibility(ctx, p.ID, Public))
	ok, err = db.CanFetch(ctx, anon, mustLoc(t, db, "a/b"))
	require.NoError(t, err)
	require.True(t, ok)

	// Member management: bob (developer) cannot; alice (owner) can; the
	// owner grant also covers deeper anchors.
	ok, err = db.CanManageMembers(ctx, bob, mustLoc(t, db, ""))
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = db.CanManageMembers(ctx, alice(t, db), mustLoc(t, db, "a/b"))
	require.NoError(t, err)
	require.True(t, ok)
	// Project admin (subproject registration): developer no, owner yes.
	ok, err = db.CanAdminProject(ctx, bob, mustLoc(t, db, ""))
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = db.CanAdminProject(ctx, alice(t, db), mustLoc(t, db, ""))
	require.NoError(t, err)
	require.True(t, ok)
}

func alice(t *testing.T, db *DB) User { return mustUser(t, db, "alice") }

// T49f: tokens — roundtrip via hash, expiry, revocation.
func TestTokens(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	u, err := db.CreateUser(ctx, "alice", false)
	require.NoError(t, err)

	secret := NewTokenSecret()
	sum := sha256Hex(secret)
	tok, err := db.CreateToken(ctx, u.ID, "ci", sum, 0)
	require.NoError(t, err)
	got, err := db.LookupToken(ctx, sum)
	require.NoError(t, err)
	require.Equal(t, "alice", got.Username)

	require.NoError(t, db.RevokeToken(ctx, tok.ID, u.ID))
	_, err = db.LookupToken(ctx, sum)
	require.ErrorIs(t, err, ErrNotFound)

	// Expiry: a timestamp in the past means the token no longer resolves.
	shortSum := sha256Hex(NewTokenSecret())
	_, err = db.CreateToken(ctx, u.ID, "short", shortSum, 1)
	require.NoError(t, err)
	_, err = db.LookupToken(ctx, shortSum)
	require.ErrorIs(t, err, ErrNotFound)

	// Username charset: dots are rejected (identity header parseability).
	_, err = db.CreateUser(ctx, "bad.name", false)
	require.ErrorIs(t, err, ErrInvalid)
	// .git-suffixed segments are reserved for the URL shape.
	_, err = db.CreateGroup(ctx, "evil.git")
	require.ErrorIs(t, err, ErrInvalid)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// T49g: the snapshot carries the raw surface — anchors with direct members,
// protected defaults, public flag — for the gateway to aggregate.
func TestBuildSnapshot(t *testing.T) {
	db := seed(t, map[string]map[string]Role{
		"":    {"alice": RoleOwner},
		"a/b": {"bob": RoleMaintainer},
	})
	ctx := context.Background()
	snap, err := db.BuildSnapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, snap.Version)
	require.ElementsMatch(t, []string{"root"}, snap.Admins)
	require.ElementsMatch(t, []string{"root", "alice", "bob", "carol"}, snap.Users)

	r, ok := snap.Repos["p_test01"]
	require.True(t, ok)
	require.False(t, r.Public)
	require.Equal(t, []string{"main"}, r.ProtectedBranches)
	require.Equal(t, map[string]string{"alice": "owner"}, r.Anchors[""].Members)
	require.Equal(t, map[string]string{"bob": "maintainer"}, r.Anchors["a/b"].Members)
	require.Empty(t, r.Anchors["a"].Members) // registered but memberless
	require.Contains(t, r.Anchors, "x/y")
}
