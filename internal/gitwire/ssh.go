// git-over-SSH transport (docs/04 §7b): the native git transport shape — one
// connection authenticates with a public key (mapped to a user at the
// auth.Service seam, docs/07 §7), then execs a git service over a session
// channel:
//
//	env GIT_PROTOCOL version=2      ← client SendEnv (v2 negotiation)
//	exec "git-upload-pack '/r/demo.git'"
//
// The serving core is shared with smart HTTP; only the session framing and
// the negotiation ROUND MODEL differ:
//
//   - v0 upload-pack is STATEFUL here: upload-pack.c answers each have
//     batch — a terminal ACK on the first have it also has (inline, at the
//     have line), NAK on every flush that arrives before any common have,
//     and NOTHING more after "done" when the ACK already went out. The
//     stateless HTTP loop batches the whole exchange into one POST instead;
//     both shapes pin to the same finishFetch.
//   - v2 upload-pack mirrors serve.c: one capability advertisement, then a
//     command loop until EOF or a bare flush-pkt.
//   - receive-pack has no v2 (git itself never defined one).
package gitwire

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/banyan-vcs/banyan/internal/auth"
)

// ServeSSH accepts SSH connections on ln until it closes, serving each on
// its own goroutine. SSH REQUIRES an Auth service — there is no key table
// without a policy, so key auth fails closed rather than pretending (the
// HTTP surface keeps its nil-means-open semantics).
func (s *Server) ServeSSH(ln net.Listener) error {
	cfg := &ssh.ServerConfig{}
	cfg.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if s.Auth == nil {
			return nil, fmt.Errorf("ssh: no authorization policy configured")
		}
		id := s.Auth.IdentifyKey(conn.User(), key.Marshal())
		if id.Anonymous() {
			return nil, fmt.Errorf("ssh: key not authorized for %q", conn.User())
		}
		return &ssh.Permissions{Extensions: map[string]string{"user": id.User}}, nil
	}
	if s.SSHHostKey != nil {
		cfg.AddHostKey(s.SSHHostKey)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serveSSHConn(c, cfg)
	}
}

// serveSSHConn runs one connection: handshake, then a single session
// channel (git opens one exec session per connection).
func (s *Server) serveSSHConn(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return // handshake failure: nothing to salvage
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		in, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		s.serveSSHSession(conn, in, requests)
	}
}

// serveSSHSession drives the session request sequence: env requests may
// precede exec (git sends GIT_PROTOCOL via SendEnv — connect.c); shell /
// pty requests are declined (this is a git service, not a login shell).
func (s *Server) serveSSHSession(conn *ssh.ServerConn, ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	id := auth.Identity{User: conn.Permissions.Extensions["user"], Name: conn.Permissions.Extensions["user"]}
	protocol := ""
	for req := range requests {
		switch req.Type {
		case "env":
			// RFC 4254 §6: string name, string value.
			name, value, ok := parseSSHEnv(req.Payload)
			if ok && name == "GIT_PROTOCOL" {
				protocol = value
			}
			req.Reply(true, nil)
		case "exec":
			req.Reply(true, nil)
			// The payload is the command in SSH string encoding (uint32
			// length + bytes) — x/crypto hands over the raw type-specific
			// bytes without unwrapping it.
			command, _, ok := parseSSHString(req.Payload)
			if !ok {
				return
			}
			code := s.serveGitExec(ch, id, protocol, command)
			// exit-status: uint32 network order (RFC 4254 §6.10) — the
			// client maps nonzero to "Could not read from remote repository".
			var status [4]byte
			status[0] = byte(code >> 24)
			ch.SendRequest("exit-status", false, status[:])
			return
		default:
			// shell, pty-req, subsystem, x11-req...: not a git concern.
			req.Reply(false, nil)
			return
		}
	}
}

// parseSSHString decodes one SSH-encoded string (uint32 length + bytes) and
// returns the rest of the buffer.
func parseSSHString(payload []byte) (s string, rest []byte, ok bool) {
	if len(payload) < 4 {
		return "", nil, false
	}
	n := int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if n < 0 || len(payload) < 4+n {
		return "", nil, false
	}
	return string(payload[4 : 4+n]), payload[4+n:], true
}

// parseSSHEnv decodes an env request payload: two uint32-length-prefixed
// strings (name, value).
func parseSSHEnv(payload []byte) (name, value string, ok bool) {
	name, payload, ok = parseSSHString(payload)
	if !ok {
		return "", "", false
	}
	value, _, ok = parseSSHString(payload)
	if !ok {
		return "", "", false
	}
	return name, value, true
}

// serveGitExec runs one exec'd git service over the channel. Returns the
// process exit status (0 = ok).
func (s *Server) serveGitExec(ch ssh.Channel, id auth.Identity, protocol, payload string) int {
	service, path, err := parseSSHCommand(payload)
	if err != nil {
		fmt.Fprintf(ch, "ERR %v\n", err)
		return 1
	}
	vr, err := ParseVirtualRepo(path)
	if err != nil {
		fmt.Fprintf(ch, "ERR %v\n", err)
		return 1
	}
	ctx := context.Background()
	br := bufio.NewReaderSize(ch, 64*1024)
	out := newBufWriter(ch)

	switch service {
	case "git-upload-pack":
		if _, err := s.Meta.GetRepo(ctx, vr.Repo); err != nil {
			fmt.Fprintf(ch, "ERR repository not found: %s\n", vr.Repo)
			return 1
		}
		if strings.Contains(protocol, "version=2") {
			return s.serveSSHUploadV2(ctx, ch, br, out, vr, id)
		}
		return s.serveSSHUploadV0(ctx, ch, br, out, vr, id)
	case "git-receive-pack":
		// Demo-friendly auto-create, gated by the push permission — same
		// rule as the HTTP discovery endpoint.
		if _, err := s.Meta.GetRepo(ctx, vr.Repo); err != nil {
			if !s.Auth.CanPush(id, vr.Repo, string(vr.Sub), "") {
				fmt.Fprintf(ch, "ERR permission denied\n")
				return 1
			}
			if err := s.Meta.CreateRepo(ctx, vr.Repo, s.Algo); err != nil {
				fmt.Fprintf(ch, "ERR create repo: %v\n", err)
				return 1
			}
			log.Printf("ssh receive-pack: auto-created repo %q", vr.Repo)
		}
		if err := s.writeRefAdvertisement(ctx, out, vr, id, service); err != nil {
			_ = out.Flush()
			return 1
		}
		_ = out.Flush()
		if err := s.finishPush(ctx, id, br, out, vr); err != nil {
			fmt.Fprintf(ch, "ERR %v\n", err)
			return 1
		}
		_ = out.Flush()
		return 0
	default:
		fmt.Fprintf(ch, "ERR unsupported service %q\n", service)
		return 1
	}
}

// serveSSHUploadV2: one advertisement, then a command loop until the client
// ends the session (serve.c protocol_v2_serve_loop).
func (s *Server) serveSSHUploadV2(ctx context.Context, ch ssh.Channel, br *bufio.Reader, out *bufWriter, vr VirtualRepo, id auth.Identity) int {
	s.advertiseV2Packets(out)
	_ = out.Flush()
	for {
		command, args, ended, err := readV2Command(br)
		if err != nil {
			_ = WritePacket(out, []byte("ERR "+err.Error()+"\n"))
			_ = WriteFlush(out)
			_ = out.Flush()
			return 1
		}
		if ended {
			return 0
		}
		switch command {
		case "ls-refs":
			s.lsRefsV2(ctx, out, vr, args, id)
		case "fetch":
			s.fetchV2(ctx, out, vr, args, id)
		default:
			_ = WritePacket(out, []byte("ERR unknown command "+command+"\n"))
			_ = WriteFlush(out)
		}
		_ = out.Flush()
	}
}

// serveSSHUploadV0: advertisement, then the stateful negotiation rounds of
// upload-pack.c get_common_commits (multi_ack off — our capability set):
//
//   - each "have" batch ends with a flush; the server answers a flush that
//     arrived before any COMMON have with NAK, and stays silent otherwise
//     (the terminal ACK already went out — see below);
//   - the first have the server also has draws an immediate terminal
//     "ACK <oid>" (upload-pack.c sends it at the have line; sending it at
//     the batch flush is equivalent for the client, which keeps one window
//     ahead and only reads ACKs from the second batch on);
//   - "done": NAK when no common have was ever found, then the pack; with
//     a common have the ACK already terminal'd the negotiation and the pack
//     follows with NO extra line;
//   - deepen requests answer the shallow section right after the wants
//     flush (send_shallow in upload-pack.c — the stateful client blocks
//     reading it before sending haves).
func (s *Server) serveSSHUploadV0(ctx context.Context, ch ssh.Channel, br *bufio.Reader, out *bufWriter, vr VirtualRepo, id auth.Identity) int {
	if err := s.writeRefAdvertisement(ctx, out, vr, id, "git-upload-pack"); err != nil {
		_ = out.Flush()
		return 1
	}
	// The advertisement must hit the wire before we block reading wants.
	_ = out.Flush()
	st := &fetchReq{}
	// Wants phase: want/shallow/deepen/filter lines until the flush that
	// ends them (receive_needs).
	for {
		line, flush, err := ReadPacketText(br)
		if err != nil {
			return 1
		}
		if flush {
			break
		}
		if msg, errPkt := applyFetchLine(st, line); msg != "" {
			if errPkt {
				_ = WritePacket(out, []byte("ERR "+msg+"\n"))
			}
			_ = out.Flush()
			return 1
		}
	}
	_ = out.Flush()
	if st.depth > 0 {
		s.writeShallowSection(out, vr.Repo, st.wants, st.clientShallow, st.depth)
		_ = out.Flush()
	}

	// Negotiation phase: have batches until done.
	acked := false
	for !st.done {
		line, flush, err := ReadPacketText(br)
		if err != nil {
			return 1 // client hung up mid-negotiation
		}
		if flush {
			if !acked {
				if common := s.firstKnown(ctx, vr.Repo, st.haves); !common.IsZero() {
					_ = WritePacket(out, []byte("ACK "+string(common)+"\n"))
					acked = true
				} else {
					_ = WritePacket(out, []byte("NAK\n"))
				}
			}
			_ = out.Flush()
			continue
		}
		if msg, errPkt := applyFetchLine(st, line); msg != "" {
			if errPkt {
				_ = WritePacket(out, []byte("ERR "+msg+"\n"))
				_ = out.Flush()
			}
			return 1
		}
	}
	if !acked {
		// done with no ACK on the wire yet: a known have terminally ACKs,
		// otherwise NAK (get_common_commits' done branch).
		if common := s.firstKnown(ctx, vr.Repo, st.haves); !common.IsZero() {
			_ = WritePacket(out, []byte("ACK "+string(common)+"\n"))
			acked = true
		} else {
			_ = WritePacket(out, []byte("NAK\n"))
		}
	}
	if len(st.wants) == 0 {
		_ = out.Flush()
		return 0
	}
	s.finishFetch(ctx, out, vr, id, st)
	_ = out.Flush()
	return 0
}

// parseSSHCommand splits an exec payload into service and repo path. The
// client builds `git-upload-pack '<path>'` (sq_quote_buf): the path is
// single-quoted with backslash-quote escapes; the "git upload-pack"
// spelling (space) is accepted for hand-written clients.
func parseSSHCommand(cmd string) (service, path string, err error) {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "git ") {
		cmd = "git-" + strings.TrimPrefix(cmd, "git ")
	}
	i := strings.IndexByte(cmd, ' ')
	if i < 0 {
		return "", "", fmt.Errorf("malformed command %q", cmd)
	}
	service, path = cmd[:i], unquoteSSHPath(strings.TrimSpace(cmd[i+1:]))
	switch service {
	case "git-upload-pack", "git-receive-pack":
	default:
		return "", "", fmt.Errorf("unsupported command %q", service)
	}
	if path == "" {
		return "", "", fmt.Errorf("empty repository path")
	}
	return service, path, nil
}

// unquoteSSHPath undoes git's sq_quote_buf: 'a b' becomes a b, and the
// escaped single quote sequence closes-quotes-reopens becomes a bare quote.
func unquoteSSHPath(p string) string {
	if len(p) >= 2 && p[0] == '\'' && p[len(p)-1] == '\'' {
		return strings.ReplaceAll(p[1:len(p)-1], `'\''`, `'`)
	}
	return p
}
