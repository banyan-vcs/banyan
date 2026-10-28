package gitaly

import (
	"bufio"
	"io"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
)

// sshService implements gitaly.SSHService's streamable RPCs: git's SSH
// services run as bidirectional sessions — stdin chunks stream in, stdout
// chunks stream back out, the exit status rides the final message (the
// workhorse git-over-SSH shape). The sidechannel upload variants share the
// yamux transport slice (docs/19 §5).
type sshService struct {
	gitalypb.UnimplementedSSHServiceServer
	s *Server
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
