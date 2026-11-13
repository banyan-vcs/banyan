package transport

import (
	"bufio"
	"context"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitaly/kit"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/gitwire"
)

// sshService implements gitaly.SSHService's streamable RPCs: git's SSH
// services run as bidirectional sessions — stdin chunks stream in, stdout
// chunks stream back out, the exit status rides the final message (the
// workhorse git-over-SSH shape). The sidechannel upload variant moves the
// session onto a yamux stream (docs/integration/gitaly-compat.md §5).
type sshService struct {
	gitalypb.UnimplementedSSHServiceServer
	*kit.Base
}

// sidebandSessionWriter frames the git session's output as pkt-line
// sideband — the server→client session format gitaly's sidechannel client
// speaks (internal/stream.ProxyPktLine: each pkt payload is a band byte +
// data; band 1 stdout, band 2 stderr, anything else is a protocol error).
// Termination is the stream close: the client's copy loop ends on EOF
// (gitlab-shell explicitly treats copy-response-EOF as benign); a flush-pkt
// would be an "invalid sideband packet" upstream.
type sidebandSessionWriter struct {
	w io.Writer
}

func (s *sidebandSessionWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := len(p)
		if n > 32768 {
			n = 32768
		}
		frame := make([]byte, 1+n)
		frame[0] = 1 // band 1: stdout
		copy(frame[1:], p[:n])
		if err := gitwire.WritePacket(s.w, frame); err != nil {
			return 0, err
		}
		p = p[n:]
	}
	return total, nil
}

// SSHUploadPackWithSidechannel runs the interactive upload-pack session over
// a yamux stream dialed back to the client: advertisement, negotiation
// rounds, pack. The client→server direction carries the raw session bytes
// (the framedConn reassembles them); the server→client direction wraps in
// the sideband framing above.
func (ss *sshService) SSHUploadPackWithSidechannel(ctx context.Context, req *gitalypb.SSHUploadPackWithSidechannelRequest) (*gitalypb.SSHUploadPackWithSidechannelResponse, error) {
	name, err := ss.ExistingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	raw, err := openSidechannel(ctx)
	if err != nil {
		return nil, err
	}
	conn := newFramedConn(raw)
	defer conn.Close() // the stream close terminates the client's copy loop
	code := ss.Git.ServeSSHSession(ctx, "git-upload-pack", "/r/"+name, req.GetGitProtocol(),
		bufio.NewReaderSize(conn, 64<<10), &sidebandSessionWriter{w: conn})
	if code != 0 {
		return nil, status.Errorf(codes.Internal, "gitaly: upload-pack session exited %d", code)
	}
	return &gitalypb.SSHUploadPackWithSidechannelResponse{}, nil
}

// sshSendWriter forwards session stdout as response chunks.
type sshSendWriter struct {
	stream gitalypb.SSHService_SSHReceivePackServer
}

func (w *sshSendWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.stream.Send(&gitalypb.SSHReceivePackResponse{Stdout: p}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (ss *sshService) SSHReceivePack(stream gitalypb.SSHService_SSHReceivePackServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	name, err := ss.ExistingRepo(ctx, first.GetRepository())
	if err != nil {
		return err
	}
	gitProtocol := first.GetGitProtocol()

	// stdin pump: remaining messages → the session reader.
	pr, pw := io.Pipe()
	if len(first.GetStdin()) > 0 {
		_, _ = pw.Write(first.GetStdin())
	}
	go func() {
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				_ = pw.Close()
				return
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(msg.GetStdin()); err != nil {
				return
			}
		}
	}()

	code := ss.Git.ServeSSHSessionGL(ctx, "git-receive-pack", "/r/"+name, gitProtocol,
		bufio.NewReaderSize(pr, 64<<10), &sshSendWriter{stream: stream},
		gitwire.GLIdentity{ID: first.GetGlId(), Repo: first.GetGlRepository(), Protocol: "ssh"})
	return stream.Send(&gitalypb.SSHReceivePackResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: int32(code)},
	})
}
