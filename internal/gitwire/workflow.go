// M4 directory-level workflow (docs/15 v2): the per-timeline job queue and
// the convergence replay. Pure Go merge (internal/merge — merge-ort subset,
// no git CLI); the queue is gateway-local and advisory (CAS stays the
// referee), convergence replays a divergent client chain onto the evolving
// combined tip with per-ticket conflict isolation.
package gitwire

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/banyan-vcs/banyan/internal/merge"
	"github.com/banyan-vcs/banyan/internal/model"
	"github.com/banyan-vcs/banyan/internal/storage"
)

// workflow carries the per-gateway queue state (lazily created on the first
// convergent push; lost on restart by design — docs/15 §2.4).
type workflow struct {
	mu     sync.Mutex
	queues map[string]*dirQueue
}

// dirQueue serializes pushes to one directory timeline. idle means "no
// drainer goroutine is running"; the submitter that flips it off becomes
// the drainer.
type dirQueue struct {
	mu   sync.Mutex
	jobs []*subtreeJob
	idle bool
}

type subtreeJob struct {
	vr      VirtualRepo
	branch  string
	old     model.OID
	newHead model.OID
	done    chan struct{}
	result  jobResult
}

type jobResult struct {
	newHead model.OID
	err     error
}

// runSubtreeJob queues one subtree push for its directory timeline and waits
// for the drainer (docs/15 §2.1). The drainer processes queued jobs FIFO —
// each converging onto the tip left by its predecessor (the merge train) —
// and exits once the queue is empty, so no goroutine lingers.
func (s *Server) runSubtreeJob(ctx context.Context, vr VirtualRepo, branch string, old, newHead model.OID) jobResult {
	s.wfOnce.Do(func() { s.wf = &workflow{queues: map[string]*dirQueue{}} })
	key := vr.Repo + "|" + string(vr.Sub) + "|" + branch
	s.wf.mu.Lock()
	q := s.wf.queues[key]
	if q == nil {
		q = &dirQueue{idle: true}
		s.wf.queues[key] = q
	}
	s.wf.mu.Unlock()

	job := &subtreeJob{vr: vr, branch: branch, old: old, newHead: newHead, done: make(chan struct{})}

	q.mu.Lock()
	take := q.idle
	if take {
		q.idle = false
	}
	q.jobs = append(q.jobs, job)
	q.mu.Unlock()
	if take {
		go s.drainQueue(q)
	}
	<-job.done
	return job.result
}

func (s *Server) drainQueue(q *dirQueue) {
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.idle = true
			q.mu.Unlock()
			return
		}
		jobs := q.jobs
		q.jobs = nil
		q.mu.Unlock()
		for _, j := range jobs {
			j.result = s.adoptOrConverge(context.Background(), j)
			close(j.done)
		}
	}
}

// adoptOrConverge lands one subtree push. Fast path (consecutive push, or
// first): today's adopt semantics verbatim. Divergent push under
// convergence: server-side replay onto the combined tip (docs/15 §2.2),
// with conflicts failing this ticket in isolation; without convergence the
// pre-M4 rejection stands.
func (s *Server) adoptOrConverge(ctx context.Context, j *subtreeJob) jobResult {
	curTip, rootLocal, err := s.Views.SubtreeHead(ctx, j.vr.Repo, j.vr.Sub, j.branch)
	if err != nil {
		return jobResult{err: err}
	}
	if curTip.IsZero() {
		return jobResult{err: fmt.Errorf("subtree view %s unavailable (root has no commits)", j.vr.Sub)}
	}
	if j.old.IsZero() || s.sameXView(j.vr, j.old, curTip) {
		if err := s.adoptSubtreePush(ctx, j.vr, j.branch, j.old, j.newHead); err != nil {
			return jobResult{err: err}
		}
		return jobResult{newHead: j.newHead}
	}

	// Root-side projection guard still applies pre-convergence (regression
	// protection the merge does not subsume): old must contain the current
	// projection tip exactly as adoptSubtreePush demands.
	if projTip, perr := s.Views.ProjectionTip(j.vr.Repo, j.vr.Sub, rootLocal); perr == nil &&
		!projTip.IsZero() && !s.Views.Descends(j.vr.Repo, j.old, projTip) && !s.sameXView(j.vr, j.old, projTip) {
		projTree := model.OID("")
		if pci, ciErr := s.Objects.CommitInfo(j.vr.Repo, projTip); ciErr == nil {
			projTree = pci.Tree
		}
		if projTree.IsZero() || !s.Views.TreeKnown(j.vr.Repo, j.old, projTree) {
			return jobResult{err: storage.ErrNonFastForward}
		}
	}

	if s.Convergence {
		head, cerr := s.convergeSubtreePush(ctx, j, curTip)
		return jobResult{newHead: head, err: cerr}
	}
	return jobResult{err: storage.ErrNonFastForward}
}

// convergeSubtreePush replays the client chain onto the combined tip and
// CASes the result. OCC: a lost CAS (another gateway landed first) refetches
// the tip and replays — bounded retries with backoff, never a client round
// trip (docs/15 §2.3).
func (s *Server) convergeSubtreePush(ctx context.Context, j *subtreeJob, curTip model.OID) (model.OID, error) {
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * 10 * time.Millisecond)
		}
		head, conflict, err := s.replayOnto(ctx, j, curTip)
		if err != nil {
			return "", err
		}
		if conflict != "" {
			return "", fmt.Errorf("conflict: %s", conflict)
		}
		localTL := model.Timeline{Repo: j.vr.Repo, Anchor: j.vr.Sub, Name: j.branch}
		expected := curTip
		if st, terr := s.Meta.Timeline(ctx, localTL); terr == nil {
			expected = st.Head
		}
		if err := s.Meta.CASHead(ctx, localTL, expected, head); err == nil {
			return head, nil
		}
		tip, _, rerr := s.Views.SubtreeHead(ctx, j.vr.Repo, j.vr.Sub, j.branch)
		if rerr != nil || tip.IsZero() {
			return "", fmt.Errorf("converge %s%s: refetch: %v", j.vr.Repo, j.vr.Sub, rerr)
		}
		curTip = tip
	}
	return "", fmt.Errorf("converge %s%s: busy", j.vr.Repo, j.vr.Sub)
}

// replayOnto synthesizes the converged chain (no CAS). The first conflicted
// path fails the ticket; synthesized-but-discarded objects are content-
// addressed garbage (harmless orphans, repack's business — same crash
// window as the two-step push, docs/10 v1 deviations).
func (s *Server) replayOnto(ctx context.Context, j *subtreeJob, curTip model.OID) (head model.OID, conflict string, err error) {
	chain, err := s.clientChain(ctx, j.vr.Repo, j.old, j.newHead)
	if err != nil {
		return "", "", err
	}
	if len(chain) == 0 {
		return j.newHead, "", nil
	}

	s.Objects.BeginSynthBatch(j.vr.Repo)
	defer s.Objects.EndSynthBatch(j.vr.Repo)

	localTL := model.Timeline{Repo: j.vr.Repo, Anchor: j.vr.Sub, Name: j.branch}
	parent := curTip
	if st, terr := s.Meta.Timeline(ctx, localTL); terr == nil {
		parent = st.Head
	}
	curInfo, err := s.Objects.CommitInfo(j.vr.Repo, curTip)
	if err != nil {
		return "", "", err
	}
	merged := s.xviewTree(j.vr, curInfo.Tree)

	store := &mergeStore{TreeStore: s.Objects.TreeStoreFor(j.vr.Repo), objects: s.Objects, repo: j.vr.Repo}
	lookup := s.Objects.Lookup(j.vr.Repo)
	prev := j.old
	for _, ci := range chain {
		info, err := lookup.Commit(ci)
		if err != nil {
			return "", "", err
		}
		prevTree := model.OID("")
		if !prev.IsZero() {
			prevInfo, perr := lookup.Commit(prev)
			if perr != nil {
				return "", "", perr
			}
			prevTree = prevInfo.TreeOID
		}
		m, conflicts, merr := merge.Trees(store, s.Algo, prevTree, merged, info.TreeOID, string(j.vr.Sub), merge.Options{
			OursLabel:   "banyan/" + j.branch,
			TheirsLabel: "converged " + shortOID(ci),
		})
		log.Printf("CONVERGE %s%s: ci=%s base=%s ours=%s theirs=%s → m=%s conflicts=%v err=%v",
			j.vr.Repo, j.vr.Sub, shortOID(ci), shortOID(prevTree), shortOID(merged), shortOID(info.TreeOID), shortOID(m), conflicts, merr)
		if merr != nil {
			return "", "", merr
		}
		if len(conflicts) > 0 {
			return "", conflicts[0], nil
		}
		// treeKnown no-op: the replay result already exists in the chain
		// history (the client pre-rebased) — reuse the client commit.
		if s.Views.TreeKnown(j.vr.Repo, parent, m) {
			parent = ci
			merged = m
			prev = ci
			continue
		}
		syn := *info
		syn.TreeOID = m
		syn.Parents = []model.OID{parent}
		syn.Committer = model.Identity{Name: "banyan", Email: "converge@banyan", When: time.Now()}
		body := syn.Encode()
		oid := syn.OID(s.Algo)
		s.Objects.PutSynthetic(j.vr.Repo, oid, model.CommitType, body)
		parent = oid
		merged = m
		prev = ci
	}
	return parent, "", nil
}

// clientChain collects the client's commits newHead..old (oldest first).
func (s *Server) clientChain(ctx context.Context, repo string, old, newHead model.OID) ([]model.OID, error) {
	var chain []model.OID
	cur := newHead
	for cur != old && cur != "" {
		chain = append(chain, cur)
		if len(chain) > 10000 {
			return nil, fmt.Errorf("converge %s: chain too long", repo)
		}
		info, err := s.Objects.CommitInfo(repo, cur)
		if err != nil {
			return nil, err
		}
		if len(info.Parents) == 0 {
			break
		}
		cur = info.Parents[0]
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// mergeStore adapts ObjectService to merge.Store: tree reads/writes through
// the repo's TreeStore, blob writes land in the synthetic-object layer.
type mergeStore struct {
	model.TreeStore
	objects *storage.ObjectService
	repo    string
}

func (m *mergeStore) PutBlob(data []byte) model.OID {
	oid := (&model.Blob{Content: data}).OID(m.objects.Algo)
	m.objects.PutSynthetic(m.repo, oid, model.BlobType, data)
	return oid
}

func shortOID(oid model.OID) string {
	if len(oid) > 8 {
		return string(oid[:8])
	}
	return string(oid)
}
