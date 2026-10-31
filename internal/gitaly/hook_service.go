package gitaly

import "context"

import (
	"strings"

	"github.com/banyan-vcs/banyan/internal/gitwire"
	"github.com/banyan-vcs/banyan/internal/gitalypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// HookService (docs/19). Upstream's hook RPCs are the loop gitaly-hooks
// closes: git spawns hook binaries inside a push, they call back into
// gitaly, gitaly reaches Rails. banyan has no git processes — its hook
// plane has always been the HTTP callback registry (docs/04 §7). These
// RPCs bridge the gitaly contract onto that registry: pre-receive and
// update evaluate fail-closed (a declined callback answers exit status 1
// with the callback's message on stderr), post-receive and the reporting
// face are best-effort. The sidechannel pair answers FailedPrecondition —
// nothing in a single-node banyan ever dials a pack-objects sidechannel.

type hookService struct {
	gitalypb.UnimplementedHookServiceServer
	s *Server
}

// hookUpdatesFromStdin parses the git hook stdin shape ("old new ref" per
// line; zero OIDs are all-zero hex).
func hookUpdatesFromStdin(stdin []byte) []gitwire.HookUpdate {
	var out []gitwire.HookUpdate
	for _, line := range strings.Split(string(stdin), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		out = append(out, gitwire.HookUpdate{Old: fields[0], New: fields[1], Ref: fields[2]})
	}
	return out
}

// envUser pulls the acting user out of the hook environment (GL_USERNAME,
// falling back to GL_ID — the identities Rails passes through).
func envUser(env []string) string {
	user, id := "", ""
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GL_USERNAME="):
			user = strings.TrimPrefix(kv, "GL_USERNAME=")
		case strings.HasPrefix(kv, "GL_ID="):
			id = strings.TrimPrefix(kv, "GL_ID=")
		}
	}
	if user == "" {
		return id
	}
	return user
}

// fireHookOutcome runs one registry evaluation and renders the wire-level
// verdict: exit 1 with the declination on stderr when a callback declines
// and the hook is fail-closed; exit 0 otherwise.
type hookVerdict struct {
	exit   int32
	stderr string
}

func evaluateHook(hs *hookService, ctx context.Context, repo, event, user string, updates []gitwire.HookUpdate, bestEffort bool) hookVerdict {
	if cerr := gitwire.FireRepoHooks(ctx, hs.s.Meta, nil, repo, event, user, updates); cerr != nil {
		if bestEffort {
			return hookVerdict{exit: 0}
		}
		return hookVerdict{exit: 1, stderr: cerr.Error()}
	}
	return hookVerdict{}
}

func (hs *hookService) PreReceiveHook(stream gitalypb.HookService_PreReceiveHookServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving first request: %v", err)
	}
		name, err := hs.s.existingRepo(stream.Context(), first.GetRepository())
	if err != nil {
		return err
	}
	stdin, env := drainHookStdin(first.GetStdin(), first.GetEnvironmentVariables(), func() ([]byte, []string, error) {
		req, rerr := stream.Recv()
		if rerr != nil {
			return nil, nil, rerr
		}
		return req.GetStdin(), req.GetEnvironmentVariables(), nil
	})
	v := evaluateHook(hs, stream.Context(), name, "pre-receive", envUser(env), hookUpdatesFromStdin(stdin), false)
	if v.stderr != "" {
		if err := stream.Send(&gitalypb.PreReceiveHookResponse{Stderr: []byte(v.stderr + "\n")}); err != nil {
			return err
		}
	}
	return stream.Send(&gitalypb.PreReceiveHookResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: v.exit}})
}

func (hs *hookService) PostReceiveHook(stream gitalypb.HookService_PostReceiveHookServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving first request: %v", err)
	}
		name, err := hs.s.existingRepo(stream.Context(), first.GetRepository())
	if err != nil {
		return err
	}
	stdin, env := drainHookStdin(first.GetStdin(), first.GetEnvironmentVariables(), func() ([]byte, []string, error) {
		req, rerr := stream.Recv()
		if rerr != nil {
			return nil, nil, rerr
		}
		return req.GetStdin(), req.GetEnvironmentVariables(), nil
	})
	v := evaluateHook(hs, stream.Context(), name, "post-receive", envUser(env), hookUpdatesFromStdin(stdin), true)
	if v.stderr != "" {
		if err := stream.Send(&gitalypb.PostReceiveHookResponse{Stderr: []byte(v.stderr + "\n")}); err != nil {
			return err
		}
	}
	return stream.Send(&gitalypb.PostReceiveHookResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: v.exit}})
}

func (hs *hookService) ReferenceTransactionHook(stream gitalypb.HookService_ReferenceTransactionHookServer) error {
	// The callback registry has no transaction-phase events (banyan's
	// writes are atomic); the hook acknowledges every state.
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving first request: %v", err)
	}
	if _, err := hs.s.existingRepo(stream.Context(), first.GetRepository()); err != nil {
		return err
	}
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
	return stream.Send(&gitalypb.ReferenceTransactionHookResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: 0}})
}

// UpdateHook evaluates the single ref under test (server-streamed stdout/
// stderr + exit status, the git update-hook contract).
func (hs *hookService) UpdateHook(in *gitalypb.UpdateHookRequest, stream gitalypb.HookService_UpdateHookServer) error {
	name, err := hs.s.existingRepo(stream.Context(), in.GetRepository())
	if err != nil {
		return err
	}
		update := gitwire.HookUpdate{
		Old: in.GetOldValue(),
		New: in.GetNewValue(),
		Ref: string(in.GetRef()),
	}
	v := evaluateHook(hs, stream.Context(), name, "update", envUser(in.GetEnvironmentVariables()),
		[]gitwire.HookUpdate{update}, false)
	if v.stderr != "" {
		if err := stream.Send(&gitalypb.UpdateHookResponse{Stderr: []byte(v.stderr + "\n")}); err != nil {
			return err
		}
	}
	return stream.Send(&gitalypb.UpdateHookResponse{
		ExitStatus: &gitalypb.ExitStatus{Value: v.exit}})
}

// ReceiveReportHook reports applied push results. banyan's reporting plane
// is the same callback registry (the post-receive event), so the report is
// delivered there, best-effort.
func (hs *hookService) ReceiveReportHook(stream gitalypb.HookService_ReceiveReportHookServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "receiving first request: %v", err)
	}
	ctx := stream.Context()
	name, err := hs.s.existingRepo(ctx, first.GetRepository())
	if err != nil {
		return err
	}
	env := first.GetEnvironmentVariables()
	var stdin []byte
	stdin = append(stdin, first.GetStdin()...)
	for {
		req, rerr := stream.Recv()
		if rerr != nil {
			break
		}
		stdin = append(stdin, req.GetStdin()...)
	}
	_ = gitwire.FireRepoHooks(ctx, hs.s.Meta, nil, name, "post-receive",
		envUser(env), hookUpdatesFromStdin(stdin))
	return stream.Send(&gitalypb.ReceiveReportHookResponse{})
}

// PackObjectsHookWithSidechannel would hand a git pack-objects process's
// stdio to the caller over a sidechannel. No git process exists in banyan
// to spawn, so the honest answer is the precondition gate — identical in
// shape to upstream's "server-side backups are not configured".
func (hs *hookService) PackObjectsHookWithSidechannel(ctx context.Context, in *gitalypb.PackObjectsHookWithSidechannelRequest) (*gitalypb.PackObjectsHookWithSidechannelResponse, error) {
	return nil, status.Error(codes.FailedPrecondition,
		"pack-objects hook: no sidechannel registry (banyan runs no git processes)")
}

// SignalPostReceiveReady tells the server the post-receive phase may
// proceed. banyan pushes are synchronous — nothing waits on the signal,
// so it acknowledges.
func (hs *hookService) SignalPostReceiveReady(ctx context.Context, in *gitalypb.SignalPostReceiveReadyRequest) (*gitalypb.SignalPostReceiveReadyResponse, error) {
	return &gitalypb.SignalPostReceiveReadyResponse{}, nil
}

// drainHookStdin collects the streamed stdin tail (the first message's
// bytes ride in directly).
func drainHookStdin(firstStdin []byte, firstEnv []string,
	recv func() ([]byte, []string, error)) ([]byte, []string) {
	stdin := append([]byte(nil), firstStdin...)
	env := firstEnv
	for {
		more, e, err := recv()
		if err != nil {
			break
		}
		stdin = append(stdin, more...)
		if len(e) > 0 {
			env = e
		}
	}
	return stdin, env
}
