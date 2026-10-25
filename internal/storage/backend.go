package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/banyan-vcs/banyan/internal/model"
)

// ShardBackend abstracts where pack bytes physically live (docs/10 §2-3):
//
//   - all-in-one: this process's local disk (LocalBackend);
//   - gateway (cluster): the scattered storage fleet (ScatteredStore) —
//     random multi-replica placement, quorum-confirmed writes, holder
//     fallback reads.
//
// PutPack applies the placement policy and returns the CONFIRMED holder IDs —
// the exact set the caller must register in metadata. Registration is the
// fact: readers only ever trust the holder set recorded in MetaStore.
type ShardBackend interface {
	PutPack(ctx context.Context, repo string, packOID model.OID, packBytes, idxBytes []byte) ([]string, error)
	// PutPackStream persists a pack from a stream (the large-push ingest
	// path): open() hands each writer an independent reader of the same
	// bytes, so no copy of the pack ever sits in memory. size feeds
	// Content-Length on the wire.
	PutPackStream(ctx context.Context, repo string, packOID model.OID, size int64, open func() (io.ReadCloser, error), idxBytes []byte) ([]string, error)
	// GetPack reads one pack, trying the given holders in order (docs/10 §3:
	// any holder may serve — the OID trailer checksum proves integrity).
	GetPack(ctx context.Context, repo string, packOID model.OID, holders []string) ([]byte, error)
	// OpenPack streams one whole pack without buffering it (exact-pack
	// verbatim serve, docs/03 §5): node disk → network → consumer in a small
	// window. The caller owns closing the reader; size is the byte length
	// when known (-1 otherwise).
	OpenPack(ctx context.Context, repo string, packOID model.OID, holders []string) (io.ReadCloser, int64, error)
	// PackSize reports a pack's byte size without transferring it (-1 when
	// unknown): the whole-pack memoize guard — big packs never ride the
	// decode-and-cache ladder.
	PackSize(ctx context.Context, repo string, packOID model.OID, holders []string) (int64, error)
	// CheapPack reports whole-pack bytes available WITHOUT a network round
	// trip — a gateway disk-cache hit (docs/11 P2-1). Object reads prefer
	// this path: one decode memoizes the whole pack in memory, so a
	// restarted gateway's first clone stops paying per-object RPCs for
	// every pack it already holds on disk. (nil, false) keeps the
	// single-object path.
	CheapPack(ctx context.Context, repo string, packOID model.OID, holders []string) ([]byte, bool)
	// GetObject extracts one object from a pack (docs/11 P1-1): the holder
	// resolves it via its local idx without transferring or decoding the
	// whole pack — the wire carries one object, not megabytes. Errors fall
	// back to the whole-pack path at the caller.
	GetObject(ctx context.Context, repo string, packOID, oid model.OID, holders []string, algo model.HashAlgo) (model.ObjectType, []byte, error)
	// DeletePack removes the pack from the given holders (repack old-shard
	// cleanup / replica trim). Missing files are tolerated.
	DeletePack(ctx context.Context, repo string, packOID model.OID, holders []string)
	// RenameRepo moves every pack between two storage keys (named
	// addressing only — hashed keys are immutable, docs/03 §6). Idempotent:
	// a holder without the source answers OK. The registry flip happens at
	// the caller AFTER this succeeds.
	RenameRepo(ctx context.Context, from, to string) error
}

// putPackStreamFromBytes adapts an in-memory pack to the streaming interface
// (small packs: synthetics, repack output, tests).
func putPackStreamFromBytes(ctx context.Context, b ShardBackend, key string, packOID model.OID, packBytes, idxBytes []byte) ([]string, error) {
	open := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(packBytes)), nil
	}
	return b.PutPackStream(ctx, key, packOID, int64(len(packBytes)), open, idxBytes)
}

// LocalBackend is the all-in-one shard backend: one local store, one node ID.
type LocalBackend struct {
	Local  *LocalShardStore
	NodeID string
}

func (b *LocalBackend) PutPack(_ context.Context, repo string, packOID model.OID, packBytes, idxBytes []byte) ([]string, error) {
	if err := b.Local.PutPack(repo, packOID, packBytes, idxBytes); err != nil {
		return nil, err
	}
	return []string{b.NodeID}, nil
}

// PutPackStream lands the pack with one streaming copy — the whole pack
// never materializes in memory (large-push ingest path).
func (b *LocalBackend) PutPackStream(_ context.Context, repo string, packOID model.OID, size int64, open func() (io.ReadCloser, error), idxBytes []byte) ([]string, error) {
	if err := b.Local.PutPackStream(repo, packOID, size, open, idxBytes); err != nil {
		return nil, err
	}
	return []string{b.NodeID}, nil
}

func (b *LocalBackend) GetPack(_ context.Context, repo string, packOID model.OID, _ []string) ([]byte, error) {
	return b.Local.GetPackBytes(repo, packOID)
}

func (b *LocalBackend) OpenPack(_ context.Context, repo string, packOID model.OID, _ []string) (io.ReadCloser, int64, error) {
	return b.Local.OpenPack(repo, packOID)
}

func (b *LocalBackend) PackSize(_ context.Context, repo string, packOID model.OID, _ []string) (int64, error) {
	return b.Local.PackFileSize(repo, packOID)
}

// CheapPack is false for the local backend: packs live on this machine, but
// the single-object idx extraction (B2) is already network-free and cheaper
// for sparse reads — only a gateway benefits from steering into whole-pack
// decode + memoize.
func (b *LocalBackend) CheapPack(_ context.Context, _ string, _ model.OID, _ []string) ([]byte, bool) {
	return nil, false
}

func (b *LocalBackend) GetObject(_ context.Context, repo string, packOID, oid model.OID, _ []string, algo model.HashAlgo) (model.ObjectType, []byte, error) {
	return b.Local.GetObject(repo, packOID, oid, algo)
}

func (b *LocalBackend) DeletePack(_ context.Context, repo string, packOID model.OID, _ []string) {
	b.Local.DeletePack(repo, packOID)
}

func (b *LocalBackend) RenameRepo(_ context.Context, from, to string) error {
	return b.Local.RenameRepo(from, to)
}

// nodeClientFor adapts a holder address to a NodeClient (ScatteredStore and
// the reconciler both need this; the map lives with the cluster pieces).
func errNoHolder(packOID model.OID, holders []string) error {
	return fmt.Errorf("%w: pack %s on %v", model.ErrObjectNotFound, packOID, holders)
}
