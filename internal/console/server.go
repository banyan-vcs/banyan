package console

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/auth"
)

// Server is the product-side control plane (docs/17 §6): the management API,
// the git-traffic front (logical {group}/{project}[/path] routing with a
// signed identity handed to the gateway), and the policy snapshot the
// gateway syncs. It never touches gateway metadata directly — project
// creation asks the gateway's own registry endpoint.
type Server struct {
	DB      *DB
	Gateway string // gateway base URL (http://host:port)
	Secret  []byte // shared with the gateway(s): identity HMAC key

	client *http.Client
}

func NewServer(db *DB, gateway string, secret []byte) *Server {
	return &Server{DB: db, Gateway: strings.TrimRight(gateway, "/"), Secret: secret,
		client: &http.Client{Timeout: 30 * time.Second}}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/", s.handleAPI)
	mux.HandleFunc("/internal/authz/policy/v2", s.handlePolicy)
	mux.HandleFunc("/", s.handleGit) // logical git URLs
}

// --- shared plumbing ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

// tokenFromRequest accepts `Bearer <pat>` or HTTP Basic where the password is
// the PAT (git's only shape). With Basic, a non-empty username must match the
// token's owner.
func tokenFromRequest(r *http.Request) (token, basicUser string, ok bool) {
	v := r.Header.Get("Authorization")
	if v == "" {
		return "", "", false
	}
	if strings.HasPrefix(v, "Bearer ") {
		return strings.TrimSpace(v[len("Bearer "):]), "", true
	}
	if user, pass, basic := basicAuth(r); basic {
		return pass, user, true
	}
	return "", "", false
}

func basicAuth(r *http.Request) (user, pass string, ok bool) {
	const prefix = "Basic "
	v := r.Header.Get("Authorization")
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", "", false
	}
	dec, err := base64.StdEncoding.DecodeString(v[len(prefix):])
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(dec), ":")
	return user, pass, ok
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// identify resolves the caller's PAT to a user. Missing or dead credentials
// yield the zero User (anonymous).
func (s *Server) identify(ctx context.Context, r *http.Request) (User, error) {
	tok, basicUser, ok := tokenFromRequest(r)
	if !ok {
		return User{}, nil
	}
	u, err := s.DB.LookupToken(ctx, HashToken(tok))
	if err != nil {
		return User{}, nil
	}
	if basicUser != "" && basicUser != u.Username {
		return User{}, nil
	}
	return u, nil
}

func requireAuth(w http.ResponseWriter, r *http.Request, u User) bool {
	if u.ID == 0 {
		w.Header().Set("WWW-Authenticate", `Basic realm="banyan"`)
		apiErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "a personal access token is required")
		return false
	}
	return true
}

// --- management API (docs/17 §6) ---

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	ctx := r.Context()
	u, err := s.identify(ctx, r)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	switch {
	case rest == "me":
		s.handleMe(w, r, u)
	case rest == "me/tokens":
		s.handleMyTokens(w, r, u)
	case rest == "users":
		s.handleUsers(w, r, u)
	case strings.HasPrefix(rest, "users/"):
		s.handleUserSub(w, r, u, strings.TrimPrefix(rest, "users/"))
	case rest == "groups":
		s.handleGroups(w, r, u)
	case rest == "repos":
		s.handleRepoList(w, r, u)
	case strings.HasPrefix(rest, "repos/"):
		s.handleRepoPath(w, r, u, strings.TrimPrefix(rest, "repos/"))
	default:
		s.proxyAPI(w, r)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", "unreadable body")
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := json.Unmarshal(body, dst); err != nil {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", "body must be JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, u User) {
	if r.Method != http.MethodGet || !requireAuth(w, r, u) {
		apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET /api/v1/me")
		return
	}
	type proj struct {
		Repo string `json:"repo"`
		Role Role   `json:"role"`
	}
	out := struct {
		User     string `json:"user"`
		Admin    bool   `json:"admin"`
		Projects []proj `json:"projects"`
	}{User: u.Username, Admin: u.Admin, Projects: []proj{}}
	for _, p := range s.reachableProjects(r.Context(), u) {
		out.Projects = append(out.Projects, proj{Repo: p.repo, Role: p.role})
	}
	writeJSON(w, http.StatusOK, out)
}

type reachable struct {
	repo string
	role Role
}

// reachableProjects lists every project the user can see with the effective
// role at its root anchor.
func (s *Server) reachableProjects(ctx context.Context, u User) []reachable {
	groups, err := s.DB.ListGroups(ctx)
	if err != nil {
		return nil
	}
	gnames := map[int64]string{}
	for _, g := range groups {
		gnames[g.ID] = g.Segment
	}
	projects, err := s.DB.ListProjects(ctx)
	if err != nil {
		return nil
	}
	var out []reachable
	for _, p := range projects {
		role, err := s.DB.EffectiveRole(ctx, u, Location{Group: Node{ID: p.ParentID}, Project: p})
		if err != nil || role == "" {
			continue
		}
		out = append(out, reachable{repo: gnames[p.ParentID] + "/" + p.Segment, role: role})
	}
	return out
}

func (s *Server) handleMyTokens(w http.ResponseWriter, r *http.Request, u User) {
	if !requireAuth(w, r, u) {
		return
	}
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		toks, err := s.DB.ListTokens(ctx, u.ID)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		type tokView struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			ExpiresAt int64  `json:"expires_at"`
			Revoked   bool   `json:"revoked"`
		}
		out := make([]tokView, 0, len(toks))
		for _, t := range toks {
			out = append(out, tokView{t.ID, t.Name, t.ExpiresAt, t.Revoked})
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
	case http.MethodPost:
		var req struct {
			Name      string `json:"name"`
			ExpiresIn int64  `json:"expires_in"` // seconds; 0 = no expiry
		}
		if !decodeBody(w, r, &req) {
			return
		}
		secret := NewTokenSecret()
		var expires int64
		if req.ExpiresIn > 0 {
			expires = time.Now().Add(time.Duration(req.ExpiresIn) * time.Second).Unix()
		}
		tok, err := s.DB.CreateToken(ctx, u.ID, req.Name, HashToken(secret), expires)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": tok.ID, "token": secret})
	case http.MethodDelete:
		id := parseID(r.URL.Query().Get("id"))
		if id == 0 {
			apiErr(w, http.StatusBadRequest, "INVALID_ARG", "?id= required")
			return
		}
		if err := s.DB.RevokeToken(ctx, id, u.ID); err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"revoked": "ok"})
	default:
		apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET/POST/DELETE")
	}
}

func parseID(s string) int64 {
	var id int64
	fmt.Sscanf(s, "%d", &id)
	return id
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, u User) {
	if !u.Admin {
		apiErr(w, http.StatusForbidden, "FORBIDDEN", "instance admin required")
		return
	}
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		users, err := s.DB.ListUsers(ctx)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		type userView struct {
			Username string `json:"username"`
			Admin    bool   `json:"admin"`
		}
		out := make([]userView, 0, len(users))
		for _, x := range users {
			out = append(out, userView{x.Username, x.Admin})
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": out})
	case http.MethodPost:
		var req struct {
			Username string `json:"username"`
			Admin    bool   `json:"admin"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		created, err := s.DB.CreateUser(ctx, req.Username, req.Admin)
		if err != nil {
			apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
			return
		}
		s.kick() // new username: personal-branch ownership
		writeJSON(w, http.StatusCreated, map[string]any{"username": created.Username, "admin": created.Admin})
	default:
		apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET/POST")
	}
}

// handleUserSub serves admin token minting: POST /users/{name}/tokens.
func (s *Server) handleUserSub(w http.ResponseWriter, r *http.Request, caller User, rest string) {
	name, tail, _ := strings.Cut(rest, "/")
	if tail != "tokens" || r.Method != http.MethodPost {
		s.proxyAPI(w, r)
		return
	}
	if !caller.Admin {
		apiErr(w, http.StatusForbidden, "FORBIDDEN", "instance admin required")
		return
	}
	var req struct {
		Name      string `json:"name"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	ctx := r.Context()
	target, err := s.DB.UserByName(ctx, name)
	if err != nil {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such user")
		return
	}
	secret := NewTokenSecret()
	var expires int64
	if req.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(req.ExpiresIn) * time.Second).Unix()
	}
	tok, err := s.DB.CreateToken(ctx, target.ID, req.Name, HashToken(secret), expires)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": tok.ID, "token": secret})
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request, u User) {
	if !requireAuth(w, r, u) {
		return
	}
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		groups, err := s.DB.ListGroups(ctx)
		if err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		names := make([]string, 0, len(groups))
		for _, g := range groups {
			names = append(names, g.Segment)
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": names})
	case http.MethodPost:
		if !u.Admin {
			apiErr(w, http.StatusForbidden, "FORBIDDEN", "instance admin required")
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		g, err := s.DB.CreateGroup(ctx, req.Name)
		if err != nil {
			apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"name": g.Segment})
	default:
		apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET/POST")
	}
}

func (s *Server) handleRepoList(w http.ResponseWriter, r *http.Request, u User) {
	if r.Method != http.MethodGet || !requireAuth(w, r, u) {
		apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET /api/v1/repos")
		return
	}
	repos := s.reachableProjects(r.Context(), u)
	out := make([]map[string]any, 0, len(repos))
	for _, p := range repos {
		out = append(out, map[string]any{"repo": p.repo, "role": string(p.role)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": out})
}

// gatewaySubResources are repo-scoped operational endpoints owned by the
// gateway; a subproject may not register on these names.
var gatewaySubResources = map[string]bool{
	"placement": true, "size": true, "hooks": true, "rename": true, "force-push": true,
}

// handleRepoPath serves /api/v1/repos/{g}[/p[/path...]][/...]: project
// lifecycle, members, subproject registration — everything else proxies to
// the gateway with the repo argument remapped.
func (s *Server) handleRepoPath(w http.ResponseWriter, r *http.Request, caller User, rest string) {
	segs := strings.Split(strings.Trim(rest, "/"), "/")

	// Project creation: POST /api/v1/repos/{group} {name}.
	if len(segs) == 1 {
		if r.Method == http.MethodPost && requireAuth(w, r, caller) {
			s.createProject(w, r, caller, segs[0])
			return
		}
		s.proxyAPI(w, r)
		return
	}

	group, project := segs[0], segs[1]
	rest2 := segs[2:]

	// Members management owns any remainder containing a "members" segment;
	// the split happens at its last occurrence.
	if n := lastIndexStr(segs[2:], "members"); n >= 0 {
		subPath := strings.Join(rest2[:n], "/")
		if len(rest2) > n+1 {
			s.memberTarget(w, r, caller, group, project, subPath, rest2[n+1])
		} else {
			s.memberList(w, r, caller, group, project, subPath)
		}
		return
	}

	switch {
	case len(rest2) == 0:
		s.projectDetail(w, r, caller, group, project)
	case r.Method == http.MethodPost:
		// POST /repos/{g}/{p}/{path...}: register a subproject — unless the
		// single segment is a gateway-owned operational subresource.
		if len(rest2) == 1 && gatewaySubResources[rest2[0]] {
			s.proxyAPI(w, r)
			return
		}
		s.registerSubproject(w, r, caller, group, project, strings.Join(rest2, "/"))
	default:
		s.proxyAPI(w, r)
	}
}

func lastIndexStr(list []string, s string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == s {
			return i
		}
	}
	return -1
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, caller User, group string) {
	var req struct {
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	ctx := r.Context()
	g, err := s.DB.GroupByName(ctx, group)
	if err != nil {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such group")
		return
	}
	uid := NewRepoUID()
	if err := s.gatewayCreateRepo(ctx, uid); err != nil {
		apiErr(w, http.StatusBadGateway, "UPSTREAM", "gateway repo creation: "+err.Error())
		return
	}
	p, err := s.DB.CreateProject(ctx, g.ID, req.Name, uid)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
		return
	}
	if req.Visibility == Public {
		if err := s.DB.SetVisibility(ctx, p.ID, Public); err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
	}
	// The creator owns what they create.
	if err := s.DB.SetMember(ctx, p.ID, caller.ID, RoleOwner); err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	s.kick()
	writeJSON(w, http.StatusCreated, map[string]any{
		"repo": group + "/" + p.Segment, "repo_uid": uid, "url": "/" + group + "/" + p.Segment + ".git",
	})
}

// gatewayCreateRepo asks the gateway's registry endpoint to create the
// physical repo, vouching as the internal "console" identity.
func (s *Server) gatewayCreateRepo(ctx context.Context, uid string) error {
	body, _ := json.Marshal(map[string]string{"name": uid})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Gateway+"/api/v1/repos", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.IdentityHeaderName, auth.SignIdentity(s.Secret, "console", time.Now()))
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// kick tells the gateway to drop its cached snapshot so the next decision
// re-syncs (management changes take effect immediately, not one TTL later).
// Synchronous — mutations are low-rate, and callers expect the grant they
// just made to hold on the very next git command.
func (s *Server) kick() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Gateway+"/internal/authz/refresh", nil)
	if err != nil {
		return
	}
	if resp, err := s.client.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (s *Server) projectDetail(w http.ResponseWriter, r *http.Request, caller User, group, project string) {
	ctx := r.Context()
	loc, err := s.DB.Resolve(ctx, group, project, "")
	if err != nil {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such project")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !requireAuth(w, r, caller) {
			return
		}
		role, _ := s.DB.EffectiveRole(ctx, caller, loc)
		if role == "" && !caller.Admin {
			apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such project") // existence hygiene
			return
		}
		dirs, _ := s.DB.ProjectDirs(ctx, loc.Project.ID)
		subs := make([]string, 0, len(dirs))
		for _, d := range dirs {
			subs = append(subs, d.Segment)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"repo": group + "/" + project, "repo_uid": loc.Project.RepoUID,
			"visibility": loc.Project.Visibility, "protected_branches": loc.Project.ProtectedBranches(),
			"subprojects": subs,
		})
	case http.MethodPatch:
		ok, err := s.DB.CanAdminProject(ctx, caller, loc)
		if err != nil || !ok {
			apiErr(w, http.StatusForbidden, "FORBIDDEN", "maintainer+ required")
			return
		}
		var req struct {
			Name              string   `json:"name"`
			Visibility        string   `json:"visibility"`
			ProtectedBranches []string `json:"protected_branches"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Name != "" && req.Name != project {
			if err := s.DB.RenameNode(ctx, loc.Project.ID, req.Name); err != nil {
				apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
				return
			}
		}
		if req.Visibility != "" {
			if err := s.DB.SetVisibility(ctx, loc.Project.ID, req.Visibility); err != nil {
				apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
				return
			}
		}
		if req.ProtectedBranches != nil {
			if err := s.DB.SetProtected(ctx, loc.Project.ID, req.ProtectedBranches); err != nil {
				apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
				return
			}
		}
		s.kick()
		writeJSON(w, http.StatusOK, map[string]string{"ok": "patched"})
	case http.MethodDelete:
		ok, err := s.DB.CanManageMembers(ctx, caller, loc) // owner gate
		if err != nil || !ok {
			apiErr(w, http.StatusForbidden, "FORBIDDEN", "owner required")
			return
		}
		if err := s.DB.ArchiveProject(ctx, loc.Project.ID); err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		s.kick()
		writeJSON(w, http.StatusOK, map[string]string{"archived": group + "/" + project})
	default:
		apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET/PATCH/DELETE")
	}
}

func (s *Server) registerSubproject(w http.ResponseWriter, r *http.Request, caller User, group, project, path string) {
	if !requireAuth(w, r, caller) {
		return
	}
	ctx := r.Context()
	loc, err := s.DB.Resolve(ctx, group, project, "")
	if err != nil {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such project")
		return
	}
	ok, err := s.DB.CanAdminProject(ctx, caller, loc)
	if err != nil || !ok {
		apiErr(w, http.StatusForbidden, "FORBIDDEN", "maintainer+ required")
		return
	}
	if segs := strings.Split(path, "/"); len(segs) == 1 && (gatewaySubResources[path] || path == "members") {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", path+" is reserved")
		return
	}
	if _, err := s.DB.CreateSubproject(ctx, loc.Project.ID, path); err != nil {
		apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
		return
	}
	s.kick()
	writeJSON(w, http.StatusCreated, map[string]string{
		"repo": group + "/" + project + "/" + path,
		"url":  "/" + group + "/" + project + "/" + path + ".git",
	})
}

// memberList: GET the direct members at one anchor.
func (s *Server) memberList(w http.ResponseWriter, r *http.Request, caller User, group, project, sub string) {
	if r.Method != http.MethodGet || !requireAuth(w, r, caller) {
		apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "GET")
		return
	}
	ctx := r.Context()
	loc, err := s.DB.Resolve(ctx, group, project, sub)
	if err != nil {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such project")
		return
	}
	role, _ := s.DB.EffectiveRole(ctx, caller, loc)
	if role == "" && !caller.Admin {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such project")
		return
	}
	members, err := s.DB.MembersOf(ctx, anchorNodeID(ctx, s.DB, loc))
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(members))
	for _, m := range members {
		out = append(out, map[string]any{"user": m.Username, "role": string(m.Role)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"anchor": sub, "members": out})
}

// memberTarget: PUT/DELETE /repos/{g}/{p}[/path]/members/{user}.
func (s *Server) memberTarget(w http.ResponseWriter, r *http.Request, caller User, group, project, sub, target string) {
	if !requireAuth(w, r, caller) {
		return
	}
	ctx := r.Context()
	loc, err := s.DB.Resolve(ctx, group, project, sub)
	if err != nil {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such project")
		return
	}
	ok, err := s.DB.CanManageMembers(ctx, caller, loc)
	if err != nil || !ok {
		apiErr(w, http.StatusForbidden, "FORBIDDEN", "owner on the chain required")
		return
	}
	tu, err := s.DB.UserByName(ctx, target)
	if err != nil {
		apiErr(w, http.StatusNotFound, "NOT_FOUND", "no such user")
		return
	}
	nodeID := anchorNodeID(ctx, s.DB, loc)
	switch r.Method {
	case http.MethodPut:
		var req struct {
			Role string `json:"role"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if err := s.DB.SetMember(ctx, nodeID, tu.ID, Role(req.Role)); err != nil {
			apiErr(w, http.StatusBadRequest, "INVALID_ARG", err.Error())
			return
		}
		s.kick()
		writeJSON(w, http.StatusOK, map[string]string{"user": target, "role": req.Role})
	case http.MethodDelete:
		if err := s.DB.RemoveMember(ctx, nodeID, tu.ID); err != nil {
			apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		s.kick()
		writeJSON(w, http.StatusOK, map[string]string{"removed": target})
	default:
		apiErr(w, http.StatusMethodNotAllowed, "INVALID_ARG", "PUT/DELETE")
	}
}

// anchorNodeID resolves the node id of a location's most specific
// registered anchor (the subproject itself when registered, else the
// project).
func anchorNodeID(ctx context.Context, db *DB, loc Location) int64 {
	if loc.Sub == "" {
		return loc.Project.ID
	}
	dirs, err := db.ProjectDirs(ctx, loc.Project.ID)
	if err != nil {
		return loc.Project.ID
	}
	for _, d := range dirs {
		if d.Segment == loc.Sub {
			return d.ID
		}
	}
	return loc.Project.ID
}

// handlePolicy serves the gateway-facing snapshot (cluster-internal face).
func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	snap, err := s.DB.BuildSnapshot(r.Context())
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}
