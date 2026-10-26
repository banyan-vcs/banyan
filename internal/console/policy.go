package console

import (
	"context"

	"github.com/banyan-vcs/banyan/internal/auth"
)

// Location is a resolved logical location: the group and project nodes plus
// a clean subtree path ("" = the repo root). Sub may point at an unregistered
// path — access then falls back along the registered prefix chain.
type Location struct {
	Group   Node
	Project Node
	Sub     string
}

// Resolve maps {group}/{project}[/sub] onto the resource tree. Archived
// projects (and anything under them) do not resolve.
func (d *DB) Resolve(ctx context.Context, group, project, sub string) (Location, error) {
	sub, err := CleanSub(sub)
	if err != nil {
		return Location{}, err
	}
	g, p, err := d.ProjectByNames(ctx, group, project)
	if err != nil {
		return Location{}, err
	}
	if p.Archived {
		return Location{}, ErrNotFound
	}
	return Location{Group: g, Project: p, Sub: sub}, nil
}

// chainIDs returns the inheritance chain for a location, shallowest first:
// the group, the project, then every registered subproject whose path is a
// prefix of Sub (most specific last). Unregistered intermediate segments
// contribute nothing (docs/17 §3).
func (d *DB) chainIDs(ctx context.Context, loc Location) ([]int64, error) {
	ids := []int64{loc.Group.ID, loc.Project.ID}
	if loc.Sub == "" {
		return ids, nil
	}
	dirs, err := d.ProjectDirs(ctx, loc.Project.ID)
	if err != nil {
		return nil, err
	}
	matched := make([]Node, 0, 4)
	for _, dir := range dirs {
		if isSubPrefix(dir.Segment, loc.Sub) {
			matched = append(matched, dir)
		}
	}
	// Order by path depth so the max-aggregation walks shallow → specific.
	// Depth gaps are fine (an unregistered intermediate): iterate to the
	// deepest matched node, not until the first empty depth.
	maxDepth := 0
	for _, n := range matched {
		if d := len(SubSegments(n.Segment)); d > maxDepth {
			maxDepth = d
		}
	}
	for target := 1; target <= maxDepth; target++ {
		for _, n := range matched {
			if len(SubSegments(n.Segment)) == target {
				ids = append(ids, n.ID)
			}
		}
	}
	return ids, nil
}

// EffectiveRole aggregates the user's direct memberships along the chain
// (additive max — docs/17 §4). Instance admins outrank everything; userID 0
// (anonymous) has no role.
func (d *DB) EffectiveRole(ctx context.Context, u User, loc Location) (Role, error) {
	if u.Admin {
		return RoleOwner, nil
	}
	if u.ID == 0 {
		return "", nil
	}
	ids, err := d.chainIDs(ctx, loc)
	if err != nil {
		return "", err
	}
	roles, err := d.RolesAt(ctx, u.ID, ids)
	if err != nil {
		return "", err
	}
	var best Role
	for _, r := range roles {
		if r.Rank() > best.Rank() {
			best = r
		}
	}
	return best, nil
}

// CanFetch decides coarse download access at a location: public projects
// serve anonymous readers; otherwise the effective role must reach Reporter.
func (d *DB) CanFetch(ctx context.Context, u User, loc Location) (bool, error) {
	if loc.Project.Visibility == Public {
		return true, nil
	}
	if u.ID == 0 {
		return false, nil
	}
	r, err := d.EffectiveRole(ctx, u, loc)
	if err != nil {
		return false, err
	}
	return r.Rank() >= RoleReporter.Rank(), nil
}

// CanPush decides coarse push access: Developer at the requested anchor.
// (Protected-branch and personal-branch precision lives in the gateway's
// snapshot enforcement, docs/17 §5.)
func (d *DB) CanPush(ctx context.Context, u User, loc Location) (bool, error) {
	if u.ID == 0 {
		return false, nil
	}
	r, err := d.EffectiveRole(ctx, u, loc)
	if err != nil {
		return false, err
	}
	return r.Rank() >= RoleDeveloper.Rank(), nil
}

// CanManageMembers reports whether the actor may edit members at a location:
// an Owner anywhere on the chain (the project itself counts), or an
// instance admin.
func (d *DB) CanManageMembers(ctx context.Context, actor User, loc Location) (bool, error) {
	if actor.Admin {
		return true, nil
	}
	r, err := d.EffectiveRole(ctx, actor, loc)
	if err != nil {
		return false, err
	}
	return r.Rank() >= RoleOwner.Rank(), nil
}

// CanAdminProject gates project lifecycle actions (rename, visibility,
// archive, protected config) and subproject registration: Maintainer or
// above on the project chain.
func (d *DB) CanAdminProject(ctx context.Context, actor User, loc Location) (bool, error) {
	if actor.Admin {
		return true, nil
	}
	r, err := d.EffectiveRole(ctx, actor, Location{Group: loc.Group, Project: loc.Project})
	if err != nil {
		return false, err
	}
	return r.Rank() >= RoleMaintainer.Rank(), nil
}

// Anchor is one addressable permission anchor of a project: the root ("")
// or a registered subproject path.
type Anchor struct {
	Path    string
	Members map[string]Role // username → direct role at this anchor
}

// Anchors lists the project's permission anchors with their direct members
// (the raw rows — aggregation happens at evaluation, in one place only).
func (d *DB) Anchors(ctx context.Context, project Node) ([]Anchor, error) {
	out := []Anchor{{Path: "", Members: map[string]Role{}}}
	dirs, err := d.ProjectDirs(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		out = append(out, Anchor{Path: dir.Segment, Members: map[string]Role{}})
	}
	for i := range out {
		var nodeID int64 = project.ID
		if out[i].Path != "" {
			for _, dir := range dirs {
				if dir.Segment == out[i].Path {
					nodeID = dir.ID
					break
				}
			}
		}
		rows, err := d.MembersOf(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		for _, m := range rows {
			out[i].Members[m.Username] = m.Role
		}
	}
	return out, nil
}

// BuildSnapshot renders the whole policy surface as the gateway-side
// snapshot v2 (docs/17 §5): usernames for personal-branch ownership, admins,
// and per-repo anchors with direct memberships. The gateway re-derives
// effective roles with the same max-over-prefix-chain formula.
func (d *DB) BuildSnapshot(ctx context.Context) (auth.UpstreamSnapshot, error) {
	snap := auth.UpstreamSnapshot{Version: 2, Repos: map[string]auth.UpstreamRepo{}}
	users, err := d.ListUsers(ctx)
	if err != nil {
		return snap, err
	}
	for _, u := range users {
		snap.Users = append(snap.Users, u.Username)
		if u.Admin {
			snap.Admins = append(snap.Admins, u.Username)
		}
	}
	projects, err := d.ListProjects(ctx)
	if err != nil {
		return snap, err
	}
	for _, p := range projects {
		anchors, err := d.Anchors(ctx, p)
		if err != nil {
			return snap, err
		}
		r := auth.UpstreamRepo{
			Public:            p.Visibility == Public,
			ProtectedBranches: p.ProtectedBranches(),
			Anchors:           map[string]auth.UpstreamAnchor{},
		}
		for _, a := range anchors {
			members := make(map[string]string, len(a.Members))
			for name, role := range a.Members {
				members[name] = string(role)
			}
			r.Anchors[a.Path] = auth.UpstreamAnchor{Members: members}
		}
		snap.Repos[p.RepoUID] = r
	}
	return snap, nil
}
