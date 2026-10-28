package gitaly

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/hashicorp/yamux"
	"google.golang.org/grpc/credentials"
)

// This file implements the gitaly connection multiplexing contract
// (upstream doc/sidechannel.md + internal/grpc/{listenmux,backchannel}):
//
//   - A sidechannel-capable client (workhorse) opens its TCP connection with
//     the literal magic "backchannel", then runs a yamux session over the
//     connection. Stream 1 (client-initiated) carries the gRPC session; the
//     server may open further streams back to the client for bulk transfer.
//   - Any other client (Rails, gitaly CLI, tests) speaks plain gRPC; the
//     credentials sniff the first bytes and pass those through untouched.
//
// The sniff lives in a TransportCredentials ServerHandshake — one listener
// serves both populations (upstream's listenmux does the same).

const backchannelMagic = "backchannel"

// muxCredentials routes each inbound connection: magic → yamux backchannel,
// anything else (the HTTP/2 preface) → plain gRPC.
type muxCredentials struct{}

func (muxCredentials) ClientHandshake(ctx context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	// The compat server never dials out with these credentials.
	return conn, muxAuthInfo{plain: true}, nil
}

func (muxCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	br := bufio.NewReaderSize(conn, 64<<10)
	head, err := br.Peek(len(backchannelMagic))
	if err == nil && string(head) == backchannelMagic {
		if _, err := br.Discard(len(backchannelMagic)); err != nil {
			return nil, nil, err
		}
		return serverBackchannel(&bufferedConn{Conn: conn, r: br})
	}
	// Plain gRPC: hand back the buffered conn so the peeked preface survives.
	return &bufferedConn{Conn: conn, r: br}, muxAuthInfo{plain: true}, nil
}

func (muxCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "gitaly-mux"}
}

func (muxCredentials) Clone() credentials.TransportCredentials { return muxCredentials{} }
func (muxCredentials) OverrideServerName(string) error         { return nil }

// serverBackchannel runs the server half of the multiplexed session and
// hands gRPC the client's stream 1.
func serverBackchannel(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = 1
	cfg.MaxStreamWindowSize = 16 << 20 // upstream's buffering posture
	session, err := yamux.Server(conn, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("gitaly: backchannel session: %w", err)
	}
	stream, err := session.Accept()
	if err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("gitaly: backchannel stream 1: %w", err)
	}
	return &backchannelConn{Conn: stream, session: session},
		muxAuthInfo{session: session}, nil
}

// backchannelConn: gRPC serves on stream 1; closing tears the whole session.
type backchannelConn struct {
	net.Conn
	session *yamux.Session
	once    sync.Once
}

func (c *backchannelConn) Close() error {
	var err error
	c.once.Do(func() { err = c.session.Close() })
	return err
}

// muxAuthInfo carries the yamux session into RPC contexts (peer.AuthInfo).
type muxAuthInfo struct {
	plain   bool
	session *yamux.Session
}

func (muxAuthInfo) AuthType() string { return "gitaly-mux" }

// bufferedConn replays peeked bytes ahead of the underlying connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

var (
	_ credentials.TransportCredentials = muxCredentials{}
	_ net.Conn                         = (*backchannelConn)(nil)
	_ net.Conn                         = (*bufferedConn)(nil)
	_ credentials.AuthInfo             = muxAuthInfo{}
)

var errNoSidechannel = errors.New("gitaly: connection does not carry a backchannel (plain gRPC client)")
