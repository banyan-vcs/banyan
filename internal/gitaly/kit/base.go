package kit

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/meta"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/service"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// Base carries the shared plane every gitaly service family rides: the
// metadata store, the object service, the service layer, the internal
// gitwire instance (smart-HTTP/SSH RPCs bridge onto it, revision resolution
// consults its advertisement) and the server options. Families embed *Base;
// helpers defined on *Base here (repo.go, treediff.go) promote into every
// family the way they used to hang off *Server when this was one package.
type Base struct {
	Meta    meta.MetaStore
	Objects *storage.ObjectService
	Algo    model.HashAlgo
	Svc     *service.Service
	Opts    Options
	Git     *gitwire.Server
	// Handler serves the internal git wire face (the root server mounts
	// the gitwire mux here; smart-HTTP/SSH RPCs bridge onto it).
	Handler http.Handler

	// SigningKey signs server auth challenges (SSH public-key exchange);
	// the root server generates one per boot (rotation = restart).
	SigningKey ed25519.PrivateKey
}

// Options configures the compat server.
type Options struct {
	// Token is the bearer token clients must present (authorization:
	// Bearer <token>); empty disables authentication (development only).
	Token string
	// GitLab roots the post-receive → internal API adapter on the internal
	// gitwire instance (pushes that carry a platform identity notify the
	// platform's push processing, docs/20). Nil = adapter off.
	GitLab *gitwire.GitLabConfig
	// StorageName is the virtual storage this node answers as — must match
	// the storage name the frontend is configured with. Default "default".
	StorageName string
	// ReportedVersion overrides ProtoVersion in ServerInfo (rare; e.g.
	// impersonating an older node).
	ReportedVersion string
	// Convergence and FullCloneMaxBytes mirror the node's workflow/budget
	// knobs onto the internal gitwire instance (push semantics + the
	// clone-budget gate apply to gitaly traffic exactly as to /r/).
	Convergence      bool
	FullCloneMaxByte int64
	// BackupDir roots the server-side backup sink (the local-directory
	// stand-in for upstream's go-cloud blob sink). Empty = server-side
	// backups off: the backup RPCs answer FailedPrecondition, like an
	// unconfigured upstream node.
	BackupDir string
}

// openSSHPublicKey renders an ed25519 public key in authorized_keys form —
// the format callers of ServerSignature expect.
func OpenSSHPublicKey(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	block := &pem.Block{Type: "PUBLIC KEY", Bytes: der}
	var out strings.Builder
	if err := pem.Encode(&out, block); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSpace(out.String())), nil
}

// ProtoVersion is the vendored gitaly protocol (internal/gitalypb) this
// server speaks. ServerInfo reports it as server_version; Rails requires a
// minimum gitaly version, so the compat layer pins to the vendored minor.
const ProtoVersion = "19.5.2"

// GitVersionReported is what ServerInfo reports as git_version. Banyan has
// no git binary — this states the git feature baseline its pure-Go protocol
// surface implements, for frontends that gate on it.
const GitVersionReported = "2.45.2"

// MaxWireMsg matches upstream's large-message posture: a fetch response or
// push body rides in few/one protobuf messages, not 4MiB defaults.
const MaxWireMsg = 1 << 30
