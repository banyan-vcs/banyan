// gitlabhook.go — the post-receive → GitLab internal API adapter (docs/integration/gitaly-compat.md
// §8's registered open gap): when banyan is a GitLab storage node, the
// platform's push processing (activity events, pipelines, MR-from-push,
// repository cache expiry) lives behind /api/v4/internal/{pre,post}_receive
// — the callback a stock gitaly runs through its hook manager. This is the
// same contract gitlab-shell speaks: a JWT (HS256, iss=gitlab-shell,
// 60s TTL) signed with the shared gitlab_shell secret in the
// Gitlab-Shell-Api-Request header, and a JSON body keyed the way Rails'
// internal API reads it. Best-effort by design: the push has already
// landed — a failing callback logs and moves on, never fails the client.
package gitwire

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/banyan-vcs/banyan/internal/model"
)

// GitLabConfig roots the adapter. Nil disables it entirely.
type GitLabConfig struct {
	URL    string // GitLab base URL as reachable from banyan (Rails behind nginx)
	Secret string // contents of GitLab's gitlab_shell_secret file
}

// GLIdentity is the GitLab push identity, threaded from the gitaly face
// (workhorse/gitlab-shell set GlId/GlRepository/GlUsername on the RPC) to
// the receive path. Zero Repo means "not a GitLab-mediated push".
type GLIdentity struct {
	ID       string // "user-<N>" / "key-<N>" / "deploy-token-<N>"
	Repo     string // "project-<N>"
	Username string
	Protocol string // "http" / "ssh"
}

// The Gl*Header consts are the internal transport the gitaly bridge uses
// to hand the identity to the gitwire handlers (they ride the synthesized
// request).
const (
	GlIDHeader       = "X-Banyan-Gitlab-Id"
	GlRepoHeader     = "X-Banyan-Gitlab-Repo"
	GlUsernameHeader = "X-Banyan-Gitlab-Username"
	GlProtocolHeader = "X-Banyan-Gitlab-Protocol"
)

func glIdentityFromHeaders(h http.Header) GLIdentity {
	id := GLIdentity{
		ID:       h.Get(GlIDHeader),
		Repo:     h.Get(GlRepoHeader),
		Username: h.Get(GlUsernameHeader),
		Protocol: h.Get(GlProtocolHeader),
	}
	if id.Repo == "" {
		return GLIdentity{} // not a GitLab push
	}
	if id.Protocol == "" {
		id.Protocol = "http"
	}
	return id
}

type gitlabNotifier struct {
	cfg GitLabConfig
	hc  *http.Client
}

func newGitlabNotifier(cfg GitLabConfig) *gitlabNotifier {
	return &gitlabNotifier{cfg: cfg, hc: &http.Client{Timeout: 5 * time.Second}}
}

// jwt signs the gitlab-shell API token: HS256 over {iss, iat, exp}
// (client/gitlabnet.go's setJWTHeader — RegisteredClaims with a 60s TTL).
func (n *gitlabNotifier) jwt(now time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(struct {
		Issuer string `json:"iss"`
		Iat    int64  `json:"iat"`
		Exp    int64  `json:"exp"`
	}{Issuer: "gitlab-shell", Iat: now.Unix(), Exp: now.Add(time.Minute).Unix()})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(strings.TrimSpace(n.cfg.Secret)))
	fmt.Fprintf(mac, "%s.%s", header, payload)
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + payload + "." + sig
}

// post signs and sends one internal API call; best-effort.
func (n *gitlabNotifier) post(ctx context.Context, path string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(n.cfg.URL, "/")+"/api/v4/internal"+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Gitlab-Shell-Api-Request", n.jwt(time.Now()))
	resp, err := n.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gitlab internal api %s: %s", path, resp.Status)
	}
	return nil
}

// preReceive pairs the reference counter Rails increments on /pre_receive
// (the only thing that endpoint does) — post_receive decrements it, so a
// push without the pair leaves the counter negative.
func (n *gitlabNotifier) preReceive(ctx context.Context, glRepo string) {
	if err := n.post(ctx, "/pre_receive", map[string]string{"gl_repository": glRepo}); err != nil {
		log.Printf("gitlab pre_receive (%s): %v", glRepo, err)
	}
}

// postReceiveChange is one applied ref in Rails' changes format: a line
// "oldrev newrev ref" (GitPostReceive#parse_changes reads them line-wise).
type postReceiveChange struct {
	Old, New, Ref string
}

// postReceive reports the applied refs; PostReceiveService enqueues the
// worker that expires caches, records events, and processes push options
// (params[:push_options] — the -o of git push, verbatim strings).
func (n *gitlabNotifier) postReceive(id GLIdentity, changes []postReceiveChange, pushOptions []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "%s %s %s\n", c.Old, c.New, c.Ref)
	}
	body := map[string]any{
		"gl_repository": id.Repo,
		"identifier":    id.ID,
		"protocol":      id.Protocol,
		"changes":       b.String(),
	}
	if len(pushOptions) > 0 {
		body["push_options"] = pushOptions
	}
	// actor.user resolves from user_id when present (Support::GitAccessActor
	// prefers it over identifier) — the worker's identify() still uses the
	// identifier string, so both ride along.
	if rest, ok := strings.CutPrefix(id.ID, "user-"); ok && digitsOnly(rest) {
		body["user_id"] = json.Number(rest)
	}
	if err := n.post(ctx, "/post_receive", body); err != nil {
		log.Printf("gitlab post_receive (%s): %v", id.Repo, err)
	}
}

func digitsOnly(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// allowedResponse is the shape every internal API verdict shares
// (internal_helpers.rb response_with_status): {status, message, …payload}.
// Status is a pointer so a missing key reads as "not allowed"; the payload
// keys ride at the top level (response_with_status merges them).
type allowedResponse struct {
	Status           *bool    `json:"status"`
	Message          string   `json:"message"`
	GitConfigOptions []string `json:"git_config_options"`
}

// allowed asks Rails whether this push may proceed — the /allowed call a
// stock gitaly runs inside its pre-receive hook. GitAccess.check(action,
// changes) evaluates repo-level access AND the ref-level policy (protected
// branches first among them); denials arrive as HTTP 401 on purpose, so the
// message rides through to the git client. The verdict payload carries
// git_config_options — receive.maxInputSize is the push size cap we enforce
// (GitLab's "Maximum push size" setting), 0 when unset.
func (n *gitlabNotifier) allowed(ctx context.Context, id GLIdentity, changes string) (ok bool, msg string, maxInput int64, err error) {
	buf, err := json.Marshal(map[string]any{
		"gl_repository": id.Repo,
		"identifier":    id.ID, // from_params: identifier covers user-N/key-N/deploy-token-N
		"protocol":      id.Protocol,
		"action":        "git-receive-pack",
		"changes":       changes, // ChangesList: newline "old new ref" lines
	})
	if err != nil {
		return false, "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(n.cfg.URL, "/")+"/api/v4/internal/allowed", bytes.NewReader(buf))
	if err != nil {
		return false, "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Gitlab-Shell-Api-Request", n.jwt(time.Now()))
	resp, err := n.hc.Do(req)
	if err != nil {
		return false, "", 0, err
	}
	defer resp.Body.Close()
	var verdict allowedResponse
	if derr := json.NewDecoder(resp.Body).Decode(&verdict); derr != nil {
		return false, "", 0, fmt.Errorf("gitlab /allowed %s: %w", resp.Status, derr)
	}
	if verdict.Status == nil || !*verdict.Status {
		return false, verdict.Message, 0, nil
	}
	for _, opt := range verdict.GitConfigOptions {
		if v, found := strings.CutPrefix(opt, "receive.maxInputSize="); found && digitsOnly(v) {
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				maxInput = n
			}
		}
	}
	return true, "", maxInput, nil
}

// glChangeOID renders a change's old/new side for Rails' line protocol.
// The zero side must ride as the full all-zero OID: ChangesList splits on
// whitespace, so an empty field shifts newrev into the ref slot and feeds
// garbage revisions to the access checks (a branch creation 500s in
// LfsCheck, which asks gitaly to resolve the ref name as a revision).
func (s *Server) glChangeOID(o model.OID) string {
	if o.IsZero() {
		return s.Algo.WireZero()
	}
	return o.String()
}

// glChanges renders updates as the "old new ref" line block both /allowed
// and /post_receive read (Gitlab::ChangesList / GitPostReceive#parse_changes).
func (s *Server) glChanges(updates []refUpdate) string {
	var b strings.Builder
	for _, u := range updates {
		fmt.Fprintf(&b, "%s %s %s\n", s.glChangeOID(u.Old), s.glChangeOID(u.New), u.Name)
	}
	return b.String()
}

// gitlabAllowed gates the receive path on GitLab's push policy. Unlike the
// fire-and-forget halves it is fail-closed — upstream behaves the same:
// Rails unreachable means no push through the platform (and pushes cannot
// reach banyan through workhorse/gitlab-shell while Rails is down anyway).
// It runs before the local pre-receive hooks and before the counter pair:
// a declined push never reaches /pre_receive. packBytes is the spooled push
// size, checked against the receive.maxInputSize the verdict carries.
func (s *Server) gitlabAllowed(ctx context.Context, vr VirtualRepo, updates []refUpdate, packBytes int64) error {
	if s.glNotif == nil || vr.GL.Repo == "" {
		return nil
	}
	ok, msg, maxInput, err := s.glNotif.allowed(ctx, vr.GL, s.glChanges(updates))
	if err != nil {
		log.Printf("gitlab allowed (%s): %v", vr.GL.Repo, err)
		return errors.New("gitlab policy check unavailable")
	}
	if !ok {
		if msg == "" {
			msg = "denied by GitLab policy"
		}
		return errors.New(msg)
	}
	if maxInput > 0 && packBytes > maxInput {
		return fmt.Errorf("pack exceeds maximum allowed size (%d bytes)", maxInput)
	}
	return nil
}

// gitlabPreReceive fires the counter-increment half (best-effort, inline:
// it is cheap and the pairing matters more than latency).
func (s *Server) gitlabPreReceive(ctx context.Context, vr VirtualRepo) {
	if s.glNotif == nil || vr.GL.Repo == "" {
		return
	}
	s.glNotif.preReceive(ctx, vr.GL.Repo)
}

// gitlabPostReceive reports the applied refs to the platform, off the push
// path — the client has its receipt already.
func (s *Server) gitlabPostReceive(vr VirtualRepo, applied []refUpdate) {
	if s.glNotif == nil || vr.GL.Repo == "" {
		return
	}
	changes := make([]postReceiveChange, 0, len(applied))
	for _, u := range applied {
		changes = append(changes, postReceiveChange{Old: s.glChangeOID(u.Old), New: s.glChangeOID(u.New), Ref: u.Name})
	}
	go s.glNotif.postReceive(vr.GL, changes, vr.PushOptions)
}
