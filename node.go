package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	ouroboros "github.com/blinklabs-io/gouroboros"
	lcommon "github.com/blinklabs-io/gouroboros/ledger/common"
	"github.com/blinklabs-io/gouroboros/protocol/localstatequery"
)

// nodeSource is a DataSource backed by a running cardano-node's
// node-to-client (N2C) Unix socket, queried through gouroboros's
// local-state-query protocol.
//
// It is *tip-derived*: the local-state-query answers at the node's live ledger
// state, so it cleanly serves the current-epoch path (the Set nonce plus the
// current pool distribution) and the chain-dep-state nonces (Go/Mark), but it
// cannot answer an arbitrary past epoch (historical points may be pruned).
// Use the koios or db sources for those.
//
// The N2C socket is local-only and unauthenticated — no TLS/cert flow (the
// …/ssl/ certs in this environment are for the TCP bridge, not this socket).
type nodeSource struct {
	conn   *ouroboros.Connection
	lsq    *localstatequery.Client
	socket string

	// The tip snapshot is fetched once per process (all interface methods
	// read the same view, so an epoch rollover mid-run cannot mix states).
	once sync.Once
	snap *nodeSnapshot
	err  error
}

// nodeSnapshot is the tip-derived input set for the leader check.
type nodeSnapshot struct {
	// CurrentEpoch is the node's tip epoch E.
	CurrentEpoch int
	// Nonces are the chain-dep-state nonces:
	//   Set  = EpochNonce          (eta0 for E)
	//   Go   = PreviousEpochNonce  (eta0 for E-1)
	//   Mark = CandidateNonce      (candidate eta0 for E+1)
	NonceSet  string // hex
	NonceGo   string // hex
	NonceMark string // hex
	// TotalActiveStake is the tip's total active stake, lovelace.
	TotalActiveStake uint64
	// Pools maps a pool's 28-byte key hash (hex) to its active stake (lovelace)
	// at the tip.
	Pools map[string]uint64
}

var _ DataSource = (*nodeSource)(nil)

// NewNodeDataSource dials the node socket and returns a ready local-state-query
// client. The connection auto-starts the client during the handshake.
func NewNodeDataSource(ctx context.Context, socketPath string, network ouroboros.Network) (DataSource, error) {
	unixConn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial node socket %q: %w", socketPath, err)
	}
	conn, err := ouroboros.NewConnection(
		ouroboros.WithConnection(unixConn),
		ouroboros.WithNetwork(network),
		ouroboros.WithLocalStateQueryConfig(localstatequery.NewConfig()),
	)
	if err != nil {
		_ = unixConn.Close()
		return nil, fmt.Errorf("ouroboros connection: %w", err)
	}
	lsq := conn.LocalStateQuery()
	if lsq == nil || lsq.Client == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("local-state-query client unavailable (node NtC version too low or protocol not enabled)")
	}
	return &nodeSource{conn: conn, lsq: lsq.Client, socket: socketPath}, nil
}

func (n *nodeSource) Name() string { return "node" }

// Close tears down the connection.
func (n *nodeSource) Close() error {
	if n.conn == nil {
		return nil
	}
	return n.conn.Close()
}

// snapshot returns the cached tip snapshot, fetching it on first use.
func (n *nodeSource) snapshot(ctx context.Context) (*nodeSnapshot, error) {
	n.once.Do(func() {
		n.snap, n.err = n.fetchSnapshot(ctx)
	})
	return n.snap, n.err
}

// fetchSnapshot acquires the node's volatile tip, reads the chain-dep-state
// nonces and the current pool distribution, then releases.
func (n *nodeSource) fetchSnapshot(ctx context.Context) (*nodeSnapshot, error) {
	var snap nodeSnapshot
	if err := n.lsq.AcquireVolatileTip(); err != nil {
		return nil, fmt.Errorf("AcquireVolatileTip: %w", err)
	}
	defer func() {
		if err := n.lsq.Release(); err != nil {
			// best-effort; the connection will be closed anyway
			_ = err
		}
	}()

	epoch, err := n.lsq.GetEpochNo()
	if err != nil {
		return nil, fmt.Errorf("GetEpochNo: %w", err)
	}
	snap.CurrentEpoch = epoch

	cds, err := n.lsq.DebugChainDepState()
	if err != nil {
		return nil, fmt.Errorf("DebugChainDepState: %w", err)
	}
	snap.NonceSet = nonceHexOrNil(cds.EpochNonce)
	snap.NonceGo = nonceHexOrNil(cds.PreviousEpochNonce)
	snap.NonceMark = nonceHexValue(&cds.CandidateNonce)

	pd, err := n.lsq.GetPoolDistr2(nil)
	if err != nil {
		return nil, fmt.Errorf("GetPoolDistr2: %w", err)
	}
	snap.TotalActiveStake = pd.TotalActiveStake
	snap.Pools = make(map[string]uint64, len(pd.Pools))
	for poolID, ips := range pd.Pools {
		key := hex.EncodeToString(poolID[:])
		snap.Pools[key] = ips.TotalPoolStake
	}
	return &snap, nil
}

func (n *nodeSource) tipDerivedError(asked int) error {
	return fmt.Errorf(
		"the node data source is tip-derived (tip epoch %d) and cannot serve epoch %d; use -data-source koios or db",
		n.snap.CurrentEpoch, asked)
}

// GetEpochInfo serves the tip epoch fully (Set nonce + current pool
// distribution totals), and the previous epoch's nonce only (its stake is not
// available at the tip, so ActiveStake is left zero — main.go rejects a zero
// total stake before any leader check). Any other epoch is an error.
func (n *nodeSource) GetEpochInfo(ctx context.Context, epoch int) (EpochInfo, error) {
	snap, err := n.snapshot(ctx)
	if err != nil {
		return EpochInfo{}, err
	}
	switch epoch {
	case snap.CurrentEpoch:
		return EpochInfo{ActiveStake: snap.TotalActiveStake, Nonce: snap.NonceSet}, nil
	case snap.CurrentEpoch - 1:
		// PreviousEpochNonce; the E-1 stake distribution is not at the tip.
		return EpochInfo{Nonce: snap.NonceGo}, nil
	}
	return EpochInfo{}, n.tipDerivedError(epoch)
}

func (n *nodeSource) GetPoolStakeForEpoch(ctx context.Context, epoch int, poolIDBech32 string) (uint64, error) {
	snap, err := n.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	if epoch != snap.CurrentEpoch {
		return 0, n.tipDerivedError(epoch)
	}
	return n.poolStakeAtTip(snap, poolIDBech32)
}

// GetStakeSnapshots serves only the "Set" entry (the tip epoch's pool
// distribution and Set nonce). The "Go"/"Mark" stake snapshots are not
// available at the tip, so their keys are absent and main.go reports them as
// unsupported instead of substituting zero values.
func (n *nodeSource) GetStakeSnapshots(ctx context.Context, poolIDBech32 string) (map[string]PoolStakeSnapshot, error) {
	snap, err := n.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	poolStake, err := n.poolStakeAtTip(snap, poolIDBech32)
	if err != nil {
		return nil, err
	}
	return map[string]PoolStakeSnapshot{
		"Set": {
			EpochNo:     snap.CurrentEpoch,
			ActiveStake: snap.TotalActiveStake,
			PoolStake:   poolStake,
			Nonce:       snap.NonceSet,
		},
	}, nil
}

// GetCandidateNonce returns the chain-dep-state candidate nonce ("" when it is
// still neutral, i.e. not yet computable for the upcoming epoch).
func (n *nodeSource) GetCandidateNonce(ctx context.Context) (string, error) {
	snap, err := n.snapshot(ctx)
	if err != nil {
		return "", err
	}
	return snap.NonceMark, nil
}

// poolStakeAtTip looks the pool up by its bech32 id (decoded to the 28-byte
// key hash the distribution is keyed by).
func (n *nodeSource) poolStakeAtTip(snap *nodeSnapshot, poolIDBech32 string) (uint64, error) {
	pid, err := lcommon.NewPoolIdFromBech32(poolIDBech32)
	if err != nil {
		return 0, fmt.Errorf("invalid pool id %q: %w", poolIDBech32, err)
	}
	stake, ok := snap.Pools[hex.EncodeToString(pid[:])]
	if !ok {
		return 0, fmt.Errorf("pool %s not found in the node's current stake distribution (retired, or no active stake)", poolIDBech32)
	}
	return stake, nil
}

// nonceHexOrNil renders a chain-dep-state nonce as 64-char hex, or "" if nil
// (e.g. the candidate nonce before the next epoch is computable).
func nonceHexOrNil(n *lcommon.Nonce) string {
	if n == nil {
		return ""
	}
	return nonceHexValue(n)
}

// nonceHexValue renders a nonce, or "" when it is a neutral (unset) nonce — the
// case for the candidate nonce before the next epoch is computable.
func nonceHexValue(n *lcommon.Nonce) string {
	if n == nil || n.Type == lcommon.NonceTypeNeutral {
		return ""
	}
	return hex.EncodeToString(n.Value[:])
}

// probeNodeSnapshot runs a one-shot probe and prints the values. Used by the
// gated test and as a template for the -data-source node path.
func probeNodeSnapshot(ctx context.Context, socketPath string, network ouroboros.Network, topN int) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ds, err := NewNodeDataSource(ctx, socketPath, network)
	if err != nil {
		return err
	}
	defer ds.Close()

	snap, err := ds.(*nodeSource).snapshot(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("socket:          %s\n", socketPath)
	fmt.Printf("network:         %s\n", network)
	fmt.Printf("current epoch:   %d\n", snap.CurrentEpoch)
	fmt.Printf("nonce Set  (E):  %s\n", snap.NonceSet)
	fmt.Printf("nonce Go   (E-1):%s\n", snap.NonceGo)
	fmt.Printf("nonce Mark (E+1):%s\n", snap.NonceMark)
	fmt.Printf("total active:    %d\n", snap.TotalActiveStake)

	// top pools by stake
	type poolStake struct {
		key string
		stk uint64
	}
	list := make([]poolStake, 0, len(snap.Pools))
	for k, v := range snap.Pools {
		list = append(list, poolStake{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].stk > list[j].stk })
	if topN > len(list) {
		topN = len(list)
	}
	fmt.Printf("pools:           %d total, top %d:\n", len(snap.Pools), topN)
	for i := 0; i < topN; i++ {
		fmt.Printf("  pool %s  stake=%d\n", list[i].key, list[i].stk)
	}
	return nil
}
