// T75 — the gitaly hook contract over banyan's callback registry
// (docs/integration/gitaly-compat.md). PreReceiveHook and UpdateHook evaluate fail-closed: a
// declined (non-2xx) callback answers exit status 1 with the callback's
// message on stderr; PostReceiveHook and ReceiveReportHook are best-effort
// deliveries; the transaction hook acknowledges; the sidechannel pair
// gates with FailedPrecondition (no git processes exist to dial back).
package gitalyrpc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/banyan-vcs/banyan/integration/support"
	"github.com/banyan-vcs/banyan/internal/gitaly"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/meta"
)

func TestT75GitalyHookService(t *testing.T) {
	support.NeedGit(t)
	s, ms, objects := support.NewServerPlane(t)
	gs := gitaly.NewServer(ms, objects, model.SHA1, gitaly.Options{Token: "t75"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := gitaly.Dial(lis.Addr().String(), "t75")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := context.Background()
	hs := gitalypb.NewHookServiceClient(conn)

	repoName := "t75-hooks"
	support.SeedRepo(t, s, repoName)
	repo := &gitalypb.Repository{StorageName: "default", RelativePath: repoName}

	var payload struct {
		Event   string `json:"event"`
		Repo    string `json:"repo"`
		User    string `json:"user"`
		Updates []struct {
			Old string `json:"old"`
			New string `json:"new"`
			Ref string `json:"ref"`
		} `json:"updates"`
	}
	var calls atomic.Int64
	verdict := atomic.Int32{} // 0 = allow, 1 = decline
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		if verdict.Load() == 1 {
			http.Error(w, "policy denied: protected branch", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	require.NoError(t, ms.SetHook(ctx, repoName, meta.HookInfo{
		ID: "policy", Events: []string{"pre-receive", "update", "post-receive"}, URL: srv.URL}))

	stdin := strings.Repeat("0", 40) + " abc123 refs/heads/main\n"
	env := []string{"GL_ID=user-42", "GL_USERNAME=dev"}

	// Allow path: exit 0, the callback saw the push shape.
	pre, err := hs.PreReceiveHook(ctx)
	require.NoError(t, err)
	require.NoError(t, pre.Send(&gitalypb.PreReceiveHookRequest{
		Repository: repo, EnvironmentVariables: env, Stdin: []byte(stdin[:20])}))
	require.NoError(t, pre.Send(&gitalypb.PreReceiveHookRequest{Stdin: []byte(stdin[20:])}))
	require.NoError(t, pre.CloseSend())
	for {
		resp, rerr := pre.Recv()
		if rerr != nil {
			break // EOF
		}
		if resp.GetExitStatus() != nil {
			assert.EqualValues(t, 0, resp.GetExitStatus().GetValue())
		}
	}
	assert.Equal(t, "pre-receive", payload.Event)
	assert.Equal(t, repoName, payload.Repo)
	assert.Equal(t, "dev", payload.User)
	require.Len(t, payload.Updates, 1)
	assert.Equal(t, "refs/heads/main", payload.Updates[0].Ref)

	// Decline path: exit 1, the callback's message rides stderr.
	verdict.Store(1)
	pre2, err := hs.PreReceiveHook(ctx)
	require.NoError(t, err)
	require.NoError(t, pre2.Send(&gitalypb.PreReceiveHookRequest{Repository: repo, EnvironmentVariables: env, Stdin: []byte(stdin)}))
	require.NoError(t, pre2.CloseSend())
	var stderrText string
	var exitCode int32
	for {
		resp, rerr := pre2.Recv()
		if rerr != nil {
			break // EOF
		}
		stderrText += string(resp.GetStderr())
		if resp.GetExitStatus() != nil {
			exitCode = resp.GetExitStatus().GetValue()
		}
	}
	assert.EqualValues(t, 1, exitCode)
	assert.Contains(t, stderrText, "policy denied")

	// Update hook: same fail-closed verdict for the single ref under test.
	upd, err := hs.UpdateHook(ctx, &gitalypb.UpdateHookRequest{
		Repository: repo, EnvironmentVariables: env,
		Ref: []byte("refs/heads/main"), OldValue: strings.Repeat("0", 40), NewValue: "abc123"})
	require.NoError(t, err)
	stderrText, exitCode = "", 0
	for {
		resp, rerr := upd.Recv()
		if rerr != nil {
			break
		}
		stderrText += string(resp.GetStderr())
		if resp.GetExitStatus() != nil {
			exitCode = resp.GetExitStatus().GetValue()
		}
	}
	assert.EqualValues(t, 1, exitCode, "update hook declines with the policy")
	assert.Contains(t, stderrText, "policy denied")

	// Post-receive: best-effort — the callback fires, the exit stays 0.
	before := calls.Load()
	post, err := hs.PostReceiveHook(ctx)
	require.NoError(t, err)
	require.NoError(t, post.Send(&gitalypb.PostReceiveHookRequest{
		Repository: repo, EnvironmentVariables: env, Stdin: []byte(stdin)}))
	require.NoError(t, post.CloseSend())
	for {
		resp2, rerr := post.Recv()
		if rerr != nil {
			break // EOF
		}
		if resp2.GetExitStatus() != nil {
			assert.EqualValues(t, 0, resp2.GetExitStatus().GetValue())
		}
	}
	assert.Equal(t, before+1, calls.Load(), "post-receive delivers to the registry")

	// ReceiveReportHook delivers the report best-effort and answers OK.
	before = calls.Load()
	rep, err := hs.ReceiveReportHook(ctx)
	require.NoError(t, err)
	require.NoError(t, rep.Send(&gitalypb.ReceiveReportHookRequest{
		Repository: repo, EnvironmentVariables: env, Stdin: []byte(stdin)}))
	require.NoError(t, rep.CloseSend())
	for {
		_, rerr := rep.Recv()
		if rerr != nil {
			break // EOF
		}
	}
	assert.Equal(t, before+1, calls.Load())

	// Transaction hook acknowledges every state.
	tx, err := hs.ReferenceTransactionHook(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.Send(&gitalypb.ReferenceTransactionHookRequest{
		Repository: repo, State: gitalypb.ReferenceTransactionHookRequest_PREPARED}))
	require.NoError(t, tx.CloseSend())
	for {
		txResp, rerr := tx.Recv()
		if rerr != nil {
			break // EOF
		}
		if txResp.GetExitStatus() != nil {
			assert.EqualValues(t, 0, txResp.GetExitStatus().GetValue())
		}
	}

	// The sidechannel pair: the honest precondition gate.
	_, err = hs.PackObjectsHookWithSidechannel(ctx, &gitalypb.PackObjectsHookWithSidechannelRequest{})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	_, err = hs.SignalPostReceiveReady(ctx, &gitalypb.SignalPostReceiveReadyRequest{})
	require.NoError(t, err)
}
