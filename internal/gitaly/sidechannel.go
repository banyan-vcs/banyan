package gitaly

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// openSidechannel dials a fresh yamux stream back to the calling client and
// performs the sidechannel confirmation exchange (upstream internal/grpc/
// sidechannel): magic "sidechannel" + big-endian sidechannel ID, then the
// client answers "ok". The returned conn carries the raw byte stream —
// stateless-rpc bodies or an interactive git session, no framing of ours.
func openSidechannel(ctx context.Context) (net.Conn, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "gitaly: no incoming metadata for sidechannel")
	}
	ids := md.Get(sidechannelMetadataKey)
	if len(ids) == 0 {
		return nil, status.Error(codes.InvalidArgument, "gitaly: sidechannel-id missing (client not sidechannel-capable)")
	}
	sidechannelID, err := strconv.ParseInt(ids[len(ids)-1], 10, 64)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "gitaly: malformed sidechannel-id")
	}

	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Internal, "gitaly: no peer info for sidechannel")
	}
	info, ok := p.AuthInfo.(muxAuthInfo)
	if !ok || info.session == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "gitaly: %v", errNoSidechannel)
	}

	stream, err := info.session.Open()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "gitaly: open sidechannel stream: %v", err)
	}
	if err := stream.SetDeadline(time.Now().Add(sidechannelOpenTimeout)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer func() { _ = stream.SetDeadline(time.Time{}) }()

	if _, err := stream.Write([]byte(sidechannelMagic)); err != nil {
		_ = stream.Close()
		return nil, status.Errorf(codes.Unavailable, "gitaly: sidechannel magic: %v", err)
	}
	if err := binary.Write(stream, binary.BigEndian, sidechannelID); err != nil {
		_ = stream.Close()
		return nil, status.Errorf(codes.Unavailable, "gitaly: sidechannel id: %v", err)
	}
	confirm := make([]byte, 2)
	if _, err := io.ReadFull(stream, confirm); err != nil {
		_ = stream.Close()
		return nil, status.Errorf(codes.Unavailable, "gitaly: sidechannel confirmation: %v", err)
	}
	if string(confirm) != "ok" {
		_ = stream.Close()
		return nil, status.Errorf(codes.Internal, "gitaly: sidechannel confirmation %q", confirm)
	}
	return stream, nil
}

const (
	sidechannelMagic       = "sidechannel"
	sidechannelMetadataKey = "gitaly-sidechannel-id"
	sidechannelOpenTimeout = 5 * time.Second
)

// The data phase is asymmetrically framed (upstream internal/grpc/sidechannel
// conn.go): client→server payloads ride length-prefixed frames ("%04x"+
// bytes, ≤65531 payload bytes) with a bare flush "0000" standing in for the
// half-close yamux cannot express; server→client bytes go through raw. The
// sidechannel conn returned to RPC handlers models exactly that.

// framedConn is the server-side view of a sidechannel stream.
type framedConn struct {
	conn net.Conn
	r    *frameReader
}

func newFramedConn(c net.Conn) *framedConn {
	return &framedConn{conn: c, r: &frameReader{r: c}}
}

func (c *framedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *framedConn) Write(p []byte) (int, error) {
	return c.conn.Write(p) // server→client: raw
}
func (c *framedConn) Close() error { return c.conn.Close() }

// frameReader reassembles the client's framed writes into a plain stream,
// ending at the half-close flush frame.
type frameReader struct {
	r   io.Reader
	buf []byte
	eof bool
}

func (fr *frameReader) Read(p []byte) (int, error) {
	for len(fr.buf) == 0 && !fr.eof {
		var hdr [4]byte
		if _, err := io.ReadFull(fr.r, hdr[:]); err != nil {
			return 0, io.EOF
		}
		n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
		if err != nil {
			return 0, fmt.Errorf("gitaly: sidechannel: malformed frame %q", hdr[:])
		}
		if n == 0 {
			fr.eof = true // flush frame = the half-close stand-in
			break
		}
		if n < 4 {
			return 0, fmt.Errorf("gitaly: sidechannel: invalid frame length %d", n)
		}
		payload := make([]byte, n-4)
		if _, err := io.ReadFull(fr.r, payload); err != nil {
			return 0, err
		}
		fr.buf = payload
	}
	if len(fr.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, fr.buf)
	fr.buf = fr.buf[n:]
	return n, nil
}
