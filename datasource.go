package main

import "context"

// EpochInfo is the per-epoch input to the leader check.
type EpochInfo struct {
	ActiveStake uint64 // total active stake, lovelace
	BlockHash   string // epoch-start block hash (hex; informational)
	Nonce       string // 32-byte epoch nonce (eta0), hex
}

// PoolStakeSnapshot is one Go/Set/Mark entry: a pool's stake plus the nonce
// and total active stake of the epoch the snapshot governs.
type PoolStakeSnapshot struct {
	EpochNo     int
	ActiveStake uint64 // total active stake, lovelace
	PoolStake   uint64 // this pool's active stake, lovelace
	Nonce       string // hex
}

// DataSource supplies the external inputs of the leader check: epoch nonces
// and (per-pool / total) active stake. The consensus core (gouroboros E34 +
// VRF) is untouched by this — only where the inputs come from varies.
//
// Implementations:
//   - koios: the Koios REST API (default; see cardano.go)
//   - db:    a cexplorer (db-sync) Postgres database (see db.go)
//   - node:  a running cardano-node's N2C socket, tip-derived (see node.go)
type DataSource interface {
	// Name returns a short identifier used in error messages ("koios", "db",
	// "node").
	Name() string

	// GetEpochInfo returns the total active stake and the epoch nonce for a
	// specific epoch. Sources that cannot serve arbitrary (past) epochs —
	// the node source is tip-derived — return an error saying so. A source
	// that cannot know the epoch's total active stake may return zero for it
	// (the leader check rejects a zero total stake before running).
	GetEpochInfo(ctx context.Context, epoch int) (EpochInfo, error)

	// GetPoolStakeForEpoch returns the pool's active stake (lovelace) for the
	// given epoch.
	GetPoolStakeForEpoch(ctx context.Context, epoch int, poolIDBech32 string) (uint64, error)

	// GetStakeSnapshots returns the pool's Go/Set/Mark snapshots, keyed by
	// those exact strings ("Go" = previous epoch, "Set" = current, "Mark" =
	// next). A source that cannot serve a snapshot — the node source only
	// has the tip's "Set" — omits the key, and the caller reports the
	// missing snapshot as unsupported rather than substituting zero values.
	GetStakeSnapshots(ctx context.Context, poolIDBech32 string) (map[string]PoolStakeSnapshot, error)

	// GetCandidateNonce returns the candidate nonce (eta0) for the upcoming
	// epoch, or "" when it is not yet computable (the caller decides how to
	// report that).
	GetCandidateNonce(ctx context.Context) (string, error)

	// Close releases the source's resources (connections, sockets).
	Close() error
}
