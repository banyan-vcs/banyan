package gitaly

import (
	"bufio"
	"context"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// sshService implements gitaly.SSHService's streamable RPCs: git's SSH
// services run as bidirectional sessions — stdin chunks stream in, stdout
// chunks stream back out, the exit status rides the final message (the
// workhorse git-over-SSH shape). The sidechannel upload variant moves the
// session onto a yamux stream (docs/19 §5).
type sshService struct {
	gitalypb.UnimplementedSSHServiceServer
	s *Server
}

// SSHUploadPackWithSidechannel runs the interactive upload-pack session over
// a yamux stream dialed back to the client: advertisement, negotiation
// rounds, pack — the raw session bytes, no gRPC message framing.
func (ss *sshService) SSHUploadPackWithSidechannel(ctx context.Context, req *gitalypb.SSHUploadPackWithSidechannelRequest) (*gitalypb.SSHUploadPackWithSidechannelResponse, error) {
	name, err := ss.s.existingRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	raw, err := openSidechannel(ctx)
	if err != nil {
		return nil, err
	}
	conn := newFramedConn(raw)
	defer conn.Close()
	code := ss.s.git.ServeSSHSession(ctx, "git-upload-pack", "/r/"+name, req.GetGitProtocol(),
		bufio.NewReaderSize(conn, 64<<10), conn)
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
	name, err := ss.s.existingRepo(ctx, first.GetRepository())
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

	code := ss.s.git.ServeSSHSession(ctx, "git-receive-pack", "/r/"+name, gitProtocol,
		bufio.NewReaderSize(pr, 64<<10), &sshSendWriter{stream: stream})
	return stream.Send(&gitalypb.SSHReceivePackResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: int32(code)},
	})
}
