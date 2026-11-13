// Package importer is banyan's server-side remote fetch: pull every
// advertised ref of a smart-HTTP remote into a repository through the SAME
// ingest path a push rides (storage.IngestPackFile + service.UpdateRef), so
// an imported repo and a pushed repo are indistinguishable afterwards.
//
// The engine grew out of the gitaly FetchRemote surface (docs/19) and is
// shared by it; the web tier's repo-import API is the other caller. SSH
// remotes are refused — the registered limitation lives in docs/19.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	gittransport "github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"

	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// rawAuth injects a verbatim Authorization header value into every
// smart-HTTP request (the same seam gitaly's kit.RawAuth provides — kept
// local so this package stands below the gitaly layer).
type rawAuth struct{ value string }

func (a rawAuth) SetAuth(r *http.Request) {
	if a.value != "" {
		r.Header.Set("Authorization", a.value)
	}
}
func (a rawAuth) Name() string   { return "raw-authorization" }
func (a rawAuth) String() string { return "raw-authorization" }

// Result reports what one Fetch landed.
type Result struct {
	// Refs is the number of refs written (heads + tags as requested).
	Refs int
	// DefaultBranch is the remote's HEAD target ("main" shape, no
	// refs/heads/ prefix); empty when the remote advertised neither a
	// symref nor an unambiguous single head.
	DefaultBranch string
}

// ErrSSHRemote mirrors the gitaly surface's registered limitation.
var ErrSSHRemote = errors.New("ssh remotes are not served (docs/19)")

// ErrInvalidURL marks a remote URL that never reached the wire (bad
// endpoint or unsupported protocol) so callers can keep their old
// invalid-argument semantics.
var ErrInvalidURL = errors.New("invalid remote url")

// Fetch pulls every advertised ref of the smart-HTTP remote into repo: one
// upload-pack exchange, the pack streamed to a spool file and ingested as
// one shard (objects never buffer in memory), refs force-set afterwards —
// the update maps an existing timeline's advertised head to the new value,
// so a re-fetch moves refs rather than conflicting.
func Fetch(ctx context.Context, objs *storage.ObjectService, svc *service.Service, repo, url, authHeader string, includeTags bool) (*Result, error) {
	if strings.HasPrefix(url, "ssh://") || strings.HasPrefix(url, "git@") {
		return nil, ErrSSHRemote
	}
	ep, err := gittransport.NewEndpoint(url)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	cl, err := transportclient.NewClient(ep)
	if err != nil {
		return nil, fmt.Errorf("%w: unsupported remote protocol %q", ErrInvalidURL, ep.Protocol)
	}
	sess, err := cl.NewUploadPackSession(ep, rawAuth{value: authHeader})
	if err != nil {
		return nil, fmt.Errorf("remote session: %w", err)
	}
	defer sess.Close()
	refs, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("remote refs: %w", err)
	}

	req := packp.NewUploadPackRequest()
	for name, hash := range refs.References {
		if name == "HEAD" || reservedRefName(name) {
			continue
		}
		if !includeTags && strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		req.Wants = append(req.Wants, hash)
	}
	if len(req.Wants) == 0 {
		return &Result{DefaultBranch: defaultBranchOf(refs)}, nil
	}
	reader, err := sess.UploadPack(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("remote upload-pack: %w", err)
	}
	defer reader.Close()

	// Spool the pack to disk, then ingest as one shard — a 2 GiB monorepo
	// import must cost the same memory as a 2 KiB one.
	dir, err := os.MkdirTemp("", "banyan-import-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	packPath := filepath.Join(dir, "remote.pack")
	f, err := os.Create(packPath)
	if err != nil {
		return nil, err
	}
	size, err := io.Copy(f, reader)
	cerr := f.Close()
	if err != nil {
		return nil, fmt.Errorf("remote pack: %w", err)
	}
	if cerr != nil {
		return nil, cerr
	}
	if _, err := objs.IngestPackFile(ctx, repo, packPath, size); err != nil {
		return nil, fmt.Errorf("remote pack rejected: %w", err)
	}

	written := 0
	for name, hash := range refs.References {
		if name == "HEAD" || !strings.HasPrefix(name, "refs/") || reservedRefName(name) {
			continue
		}
		if !includeTags && strings.HasPrefix(name, "refs/tags/") {
			continue
		}
		if err := svc.UpdateRef(ctx, service.RefUpdate{
			Repo: repo, Name: name, New: model.OID(hash.String())}); err != nil {
			return nil, err
		}
		written++
	}
	return &Result{Refs: written, DefaultBranch: defaultBranchOf(refs)}, nil
}

// reservedRefName reports the banyan-local namespaces an import must not
// copy: refs/banyan/* is DERIVED state (subtree projections) — the target
// repo derives its own from the imported timelines, a verbatim copy would
// shadow them with phantom aux refs (the UpdateRef guard agrees).
func reservedRefName(name string) bool {
	return strings.HasPrefix(name, "refs/banyan/")
}

// defaultBranchOf reads the advertisement's symref capability (HEAD →
// refs/heads/<name>), falling back to the only head when there is exactly
// one — the same probe FindRemoteRootRef serves on the gitaly face.
func defaultBranchOf(refs *packp.AdvRefs) string {
	for _, symref := range refs.Capabilities.Get(capability.SymRef) {
		if head, target, ok := strings.Cut(symref, ":"); ok && head == "HEAD" {
			if _, exists := refs.References[target]; exists {
				return strings.TrimPrefix(target, "refs/heads/")
			}
		}
	}
	var onlyHead string
	for name := range refs.References {
		if strings.HasPrefix(name, "refs/heads/") {
			if onlyHead != "" {
				return ""
			}
			onlyHead = name
		}
	}
	return strings.TrimPrefix(onlyHead, "refs/heads/")
}
