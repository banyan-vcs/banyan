package storage

import (
	"context"
	"fmt"

	"github.com/banyan-vcs/banyan/internal/model"
)

// ShardBackend abstracts where pack bytes physically live (docs/11 §2-3):
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
	// GetPack reads one pack, trying the given holders in order (docs/11 §3:
	// any holder may serve — the OID trailer checksum proves integrity).
	GetPack(ctx context.Context, repo string, packOID model.OID, holders []string) ([]byte, error)
	// CheapPack reports whole-pack bytes available WITHOUT a network round
	// trip — a gateway disk-cache hit (docs/12 P2-1). Object reads prefer
	// this path: one decode memoizes the whole pack in memory, so a
	// restarted gateway's first clone stops paying per-object RPCs for
	// every pack it already holds on disk. (nil, false) keeps the
	// single-object path.
	CheapPack(ctx context.Context, repo string, packOID model.OID, holders []string) ([]byte, bool)
	// GetObject extracts one object from a pack (docs/12 P1-1): the holder
	// resolves it via its local idx without transferring or decoding the
	// whole pack — the wire carries one object, not megabytes. Errors fall
	// back to the whole-pack path at the caller.
	GetObject(ctx context.Context, repo string, packOID, oid model.OID, holders []string, algo model.HashAlgo) (model.ObjectType, []byte, error)
	// DeletePack removes the pack from the given holders (repack old-shard
	// cleanup / replica trim). Missing files are tolerated.
	DeletePack(ctx context.Context, repo string, packOID model.OID, holders []string)
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

func (b *LocalBackend) GetPack(_ context.Context, repo string, packOID model.OID, _ []string) ([]byte, error) {
	return b.Local.GetPackBytes(repo, packOID)
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

// nodeClientFor adapts a holder address to a NodeClient (ScatteredStore and
// the reconciler both need this; the map lives with the cluster pieces).
func errNoHolder(packOID model.OID, holders []string) error {
	return fmt.Errorf("%w: pack %s on %v", model.ErrObjectNotFound, packOID, holders)
}
