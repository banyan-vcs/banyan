package transport

import (
	"bufio"
	"bytes"
	"fmt"
	"io"

	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/gitwire"
)

// SSHUploadArchive (docs/19) speaks git's upload-archive wire protocol —
// the exchange `git archive --remote` drives:
//
//	client → server: pkt-lines "argument <arg>"…, flush-pkt
//	server → client: pkt-line "ACK\n", flush-pkt, then the archive
//	                 multiplexed on sideband 1 (band 2 = progress, band 3 =
//	                 fatal error), closed by a final flush-pkt
//
// The archive bytes come from the same writers GetArchive serves, so the
// stream is byte-identical to `git archive` of the same tree (T76). Bad
// arguments answer the protocol's NACK form.

// uploadArchiveSideband is the largest payload per sideband pkt-line
// (git's LARGE_PACKET_MAX minus the band byte and header).
const uploadArchiveSideband = 65515

func (ss *sshService) SSHUploadArchive(stream gitalypb.SSHService_SSHUploadArchiveServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving first request: %v", err)
	}
	name, err := ss.ExistingRepo(ctx, first.GetRepository())
	if err != nil {
		return err
	}
	var stdin []byte
	stdin = append(stdin, first.GetStdin()...)
	for {
		msg, rerr := stream.Recv()
		if rerr != nil {
			break // EOF (or transport end): the request is whole
		}
		stdin = append(stdin, msg.GetStdin()...)
	}

	args, aerr := parseUploadArchiveArgs(stdin)
	if aerr != nil {
		return ss.uploadArchiveNACK(stream, aerr.Error())
	}

	format, prefix, rev, ferr := uploadArchiveArgsOf(args)
	if ferr != nil {
		return ss.uploadArchiveNACK(stream, ferr.Error())
	}

	// ACK first, then the archive rides sideband 1, flush-terminated.
	hello := &bytes.Buffer{}
	if err := writePktLine(hello, "ACK\n"); err != nil {
		return err
	}
	if err := gitwire.WriteFlush(hello); err != nil {
		return err
	}
	if err := stream.Send(&gitalypb.SSHUploadArchiveResponse{Stdout: hello.Bytes()}); err != nil {
		return err
	}

	archive := &bytes.Buffer{}
	if rerr := ss.RenderArchive(ctx, name,
		uploadArchiveFormat(format), rev, prefix, archive); rerr != nil {
		// The child-death form: the error rides band 3 and the exchange
		// still terminates with the final flush.
		_ = stream.Send(&gitalypb.SSHUploadArchiveResponse{
			Stdout: sidebandPacket(3, []byte(rerr.Error()))})
		_ = stream.Send(&gitalypb.SSHUploadArchiveResponse{Stdout: flushPktBytes()})
		return stream.Send(&gitalypb.SSHUploadArchiveResponse{
			ExitStatus: &gitalypb.ExitStatus{Value: 1}})
	}
	archiveBytes := archive.Bytes()
	for off := 0; off < len(archiveBytes); off += uploadArchiveSideband {
		end := off + uploadArchiveSideband
		if end > len(archiveBytes) {
			end = len(archiveBytes)
		}
		pkt := sidebandPacket(1, archiveBytes[off:end])
		if err := stream.Send(&gitalypb.SSHUploadArchiveResponse{Stdout: pkt}); err != nil {
			return err
		}
	}
	tail := flushPktBytes()
	if err := stream.Send(&gitalypb.SSHUploadArchiveResponse{Stdout: tail}); err != nil {
		return err
	}
	return stream.Send(&gitalypb.SSHUploadArchiveResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: 0}})
}

// uploadArchiveFormat maps the wire format name onto the archive enum.
func uploadArchiveFormat(format string) gitalypb.GetArchiveRequest_Format {
	if format == "zip" {
		return gitalypb.GetArchiveRequest_ZIP
	}
	if format == "tar.gz" {
		return gitalypb.GetArchiveRequest_TAR_GZ
	}
	return gitalypb.GetArchiveRequest_TAR
}

// flushPktBytes renders the terminating flush-pkt.
func flushPktBytes() []byte {
	var b bytes.Buffer
	_ = gitwire.WriteFlush(&b)
	return b.Bytes()
}

// uploadArchiveNACK answers the argument-error form: a NACK pkt-line with
// the reason, then exit status 1.
func (ss *sshService) uploadArchiveNACK(stream gitalypb.SSHService_SSHUploadArchiveServer, reason string) error {
	out := &bytes.Buffer{}
	_ = writePktLine(out, "NACK "+reason+"\n")
	_ = gitwire.WriteFlush(out)
	if err := stream.Send(&gitalypb.SSHUploadArchiveResponse{Stdout: out.Bytes()}); err != nil {
		return err
	}
	return stream.Send(&gitalypb.SSHUploadArchiveResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: 1}})
}

// parseUploadArchiveArgs reads the client's argument block: pkt-lines of
// "argument <arg>" until the flush.
func parseUploadArchiveArgs(stdin []byte) ([]string, error) {
	r := bufio.NewReader(bytes.NewReader(stdin))
	var args []string
	for {
		payload, flush, err := gitwire.ReadPacket(r)
		if err != nil {
			return nil, fmt.Errorf("reading request: %w", err)
		}
		if flush {
			return args, nil
		}
		line := strings.TrimRight(string(payload), "\n")
		arg, ok := strings.CutPrefix(line, "argument ")
		if !ok {
			return nil, fmt.Errorf("'argument' token or flush expected")
		}
		args = append(args, arg)
	}
}

// uploadArchiveArgsOf folds the argument vector into the archive request:
// --format, --prefix and the trailing revision (git archive's own shape).
func uploadArchiveArgsOf(args []string) (format, prefix, rev string, err error) {
	format = "tar"
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--format="):
			format = strings.TrimPrefix(args[i], "--format=")
		case strings.HasPrefix(args[i], "--prefix="):
			prefix = strings.TrimPrefix(args[i], "--prefix=")
		case args[i] == "-0" || args[i] == "-9":
			// compression hint levels: served formats handle it
		default:
			if rev == "" && !strings.HasPrefix(args[i], "-") {
				rev = args[i]
			} else {
				return "", "", "", fmt.Errorf("unsupported argument %q", args[i])
			}
		}
	}
	if rev == "" {
		return "", "", "", fmt.Errorf("no revision given")
	}
	switch format {
	case "tar", "tar.gz":
		return format, prefix, rev, nil
	default:
		return "", "", "", fmt.Errorf("unknown archive format %q", format)
	}
}

func writePktLine(w io.Writer, s string) error {
	return gitwire.WritePacket(w, []byte(s))
}

// sidebandPacket frames one payload on a sideband.
func sidebandPacket(band byte, payload []byte) []byte {
	total := len(payload) + 5 // header + band byte + payload
	head := []byte(fmt.Sprintf("%04x", total))
	out := make([]byte, 0, total)
	out = append(out, head...)
	out = append(out, band)
	out = append(out, payload...)
	return out
}
