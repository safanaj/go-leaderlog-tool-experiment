package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ouroboros "github.com/blinklabs-io/gouroboros"
	consensus "github.com/blinklabs-io/gouroboros/consensus"
	"github.com/blinklabs-io/gouroboros/vrf"
	flag "github.com/spf13/pflag"
)

var (
	vrfSKeyCborHex, poolIdBech32, tz    string
	onlyNonce, all, prev, current, next bool
	parallelFactor, epochNo             int
	consensusModeStr                    string
	fStr                                string

	dataSourceStr                string
	dbDSN, dbCertFile, dbKeyFile string
	dbCACertFile                 string
	nodeSocket, nodeNetwork      string

	tzLoc *time.Location
)

// defaultDBDSN is the default Postgres DSN for -data-source db (a cexplorer
// database reached with client-certificate TLS; see PLAN.md).
const defaultDBDSN = "host=marge.bardels.me port=5432 dbname=cexplorer user=cardano sslmode=verify-ca"

func parseFlags() {
	flag.StringVar(&vrfSKeyCborHex, "vrf-skey-cborhex", "", "VRF skey as CBOR hex")
	flag.StringVar(&poolIdBech32, "pool-id-bech32", "", "Pool id as bech32")
	flag.StringVar(&tz, "tz", "Local", "Time zone for the output")
	flag.StringVar(&consensusModeStr, "consensus", "cpraos", "Consensus mode: cpraos, tpraos, praos")
	flag.StringVar(&fStr, "f", "1/20", "Active slot coefficient f as a rational (e.g. 1/20 or 0.05)")
	flag.BoolVar(&onlyNonce, "only-nonce", false, "just get nonce")
	flag.BoolVar(&all, "all", false, "compute for all epochs")
	flag.BoolVar(&prev, "prev", false, "compute for previous epoch")
	flag.BoolVar(&current, "current", true, "compute for current epoch")
	flag.BoolVar(&next, "next", false, "compute for next epoch")
	flag.IntVar(&parallelFactor, "parallel-factor", 30, "concurrency = NumCPU * this")
	flag.IntVar(&epochNo, "epoch-no", 0, "compute for epoch (a past one)")

	flag.StringVar(&dataSourceStr, "data-source", "koios", "data source: koios, db, or node")
	flag.StringVar(&dbDSN, "db-dsn", defaultDBDSN, "Postgres DSN (keyword format) for -data-source db")
	flag.StringVar(&dbCertFile, "db-cert-file", "", "client TLS certificate for -data-source db (sslcert); env DB_CERT_FILE")
	flag.StringVar(&dbKeyFile, "db-key-file", "", "client TLS key for -data-source db (sslkey); env DB_KEY_FILE")
	flag.StringVar(&dbCACertFile, "db-ca-cert-file", "", "CA certificate for -data-source db (sslrootcert); env DB_CA_CERT_FILE")
	flag.StringVar(&nodeSocket, "node-socket", "", "cardano-node node-to-client socket for -data-source node; env NODE_SOCKET")
	flag.StringVar(&nodeNetwork, "node-network", "mainnet", "network for -data-source node: mainnet, preprod, preview")

	flag.Parse()

	// env-var overrides for the data-source flags (flag > env > default)
	dbDSN = firstNonEmpty(dbDSN, os.Getenv("DB_DSN"))
	dbCertFile = firstNonEmpty(dbCertFile, os.Getenv("DB_CERT_FILE"))
	dbKeyFile = firstNonEmpty(dbKeyFile, os.Getenv("DB_KEY_FILE"))
	dbCACertFile = firstNonEmpty(dbCACertFile, os.Getenv("DB_CA_CERT_FILE"))
	nodeSocket = firstNonEmpty(nodeSocket, os.Getenv("NODE_SOCKET"))
	nodeNetwork = firstNonEmpty(nodeNetwork, os.Getenv("NODE_NETWORK"))

	if (next || prev) && !all {
		current = false
	}
}

// firstNonEmpty returns the first non-empty value (flag > env).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// newDataSource builds the DataSource selected by -data-source (default:
// koios).
func newDataSource(ctx context.Context) (DataSource, error) {
	switch strings.ToLower(strings.TrimSpace(dataSourceStr)) {
	case "", "koios":
		return NewKoiosDataSource(ctx)
	case "db", "postgres":
		return NewDBDataSource(ctx, &dbConfig{
			DSN:    dbDSN,
			Cert:   dbCertFile,
			Key:    dbKeyFile,
			CACert: dbCACertFile,
		})
	case "node":
		network, err := parseNetwork(nodeNetwork)
		if err != nil {
			return nil, err
		}
		if nodeSocket == "" {
			return nil, fmt.Errorf("-node-socket is required for -data-source node")
		}
		return NewNodeDataSource(ctx, nodeSocket, network)
	default:
		return nil, fmt.Errorf("unknown -data-source %q (want koios, db, or node)", dataSourceStr)
	}
}

// parseNetwork maps the -node-network flag to a gouroboros network. The
// legacy "testnet" name maps to preprod (the classic testnet is retired and
// not defined by gouroboros).
func parseNetwork(s string) (ouroboros.Network, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "mainnet":
		return ouroboros.NetworkCardanoMainnet, nil
	case "preprod", "preproduction", "testnet":
		return ouroboros.NetworkCardanoPreprod, nil
	case "preview":
		return ouroboros.NetworkCardanoPreview, nil
	default:
		return ouroboros.Network{}, fmt.Errorf("unknown -node-network %q (want mainnet, preprod, or preview)", s)
	}
}

// parseMode maps the -consensus flag to gouroboros's ConsensusMode. "praos"
// and "cpraos" both mean the current (Babbage+) CPraos protocol; "tpraos"
// means the transitional Shelley-Alonzo protocol.
func parseMode(s string) (consensus.ConsensusMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "praos", "cpraos", "cpbraos":
		return consensus.ConsensusModeCPraos, nil
	case "tpraos", "tpbraos":
		return consensus.ConsensusModeTPraos, nil
	default:
		return 0, fmt.Errorf("unknown consensus mode %q", s)
	}
}

// parseRat parses an active slot coefficient. Accepts "num/den" fraction form
// (e.g. "1/20") or a decimal string (e.g. "0.05"). The exact rational is used
// so the E34 predicate matches cardano-node.
func parseRat(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i > 0 {
		num, ok1 := new(big.Int).SetString(strings.TrimSpace(s[:i]), 10)
		den, ok2 := new(big.Int).SetString(strings.TrimSpace(s[i+1:]), 10)
		if !ok1 || !ok2 || den.Sign() == 0 {
			return nil, fmt.Errorf("invalid fraction %q", s)
		}
		return new(big.Rat).SetFrac(num, den), nil
	}
	r := new(big.Rat)
	if _, ok := r.SetString(s); !ok {
		return nil, fmt.Errorf("invalid rational %q", s)
	}
	return r, nil
}

type assignedSlot struct {
	Epoch, Slot, SlotInEpoch int
	Time                     string
}

type leaderSummary struct {
	Epoch          int
	PoolStake      string
	TotalStake     string
	PoolStakeInADA string
	Nonce          string
	Consensus      string
	F              string
	Ideal          float64
	Performance    float64
	AssignedSlots  []*assignedSlot
}

// checkSlotsInEpoch evaluates the PRAOS leader predicate for every slot of the
// epoch using gouroboros's finite E34 predicate (matching cardano-node), the
// same primitive Dingo's node uses.
//
// Any failure while evaluating a slot (VRF input, prove, eligibility check)
// aborts the sweep and is returned: a transient error must never silently
// drop an assigned slot from the log.
func checkSlotsInEpoch(
	e, firstSlot int,
	poolStake, totalStake uint64,
	nonce string,
	f *big.Rat,
	mode consensus.ConsensusMode,
	vrfSigner consensus.VRFSigner,
) error {
	if firstSlot < int(FirstShelleySlot) {
		return fmt.Errorf("epoch %d is pre-Shelley; leader logs start at epoch %d", e, FirstShelleyEpoch)
	}
	if totalStake == 0 {
		// A zero total stake would silently produce an empty leader log (the
		// E34 predicate never fires); it means the data source could not serve
		// the epoch's stake (e.g. the tip-derived node source for a past epoch).
		return fmt.Errorf("total active stake for epoch %d is zero — the data source returned no stake; refusing to compute a necessarily-empty leader log", e)
	}
	nb, err := nonceBytes(nonce)
	if err != nil {
		return err
	}
	nonceB := nb[:]

	elig, err := consensus.NewLeaderEligibility(poolStake, totalStake, f, mode)
	if err != nil {
		return fmt.Errorf("preparing leader eligibility: %w", err)
	}

	concurrency := runtime.NumCPU() * parallelFactor
	if concurrency < 1 {
		concurrency = 1
	}
	var wg sync.WaitGroup
	ch := make(chan struct{}, concurrency)
	slotsCh := make(chan *assignedSlot)

	fmt.Printf("epoch: %d - first_slot_of_epoch: %d - first_slot_of_prev_epoch: %d\n",
		e, firstSlot, firstSlot-EpochLength)
	fmt.Printf("eta0: %s\n", nonceHex(nb))

	stakeRatio := float64(poolStake) / float64(totalStake)
	fFloat, _ := f.Float64()
	ideal := stakeRatio * float64(EpochLength) * fFloat

	ls := &leaderSummary{
		Epoch:          e,
		Nonce:          nonceHex(nb),
		Consensus:      consensusModeName(mode),
		F:              f.RatString(),
		PoolStake:      fmt.Sprint(poolStake),
		TotalStake:     fmt.Sprint(totalStake),
		PoolStakeInADA: fmt.Sprintf("%.6f", float64(poolStake)/1e6),
		Ideal:          ideal,
	}

	// assigned-slots collector; we wait for it to drain before reading the
	// slice it appends to.
	collectorDone := make(chan struct{})
	go func() {
		defer close(collectorDone)
		for as := range slotsCh {
			ls.AssignedSlots = append(ls.AssignedSlots, as)
		}
	}()

	// First error wins; later failures are dropped. errCh has room for the
	// single CAS winner, so fail never blocks.
	var failed atomic.Bool
	errCh := make(chan error, 1)
	fail := func(slot int, err error) {
		if failed.CompareAndSwap(false, true) {
			errCh <- fmt.Errorf("slot %d: %w", slot, err)
		}
	}

	fmt.Printf("slots range: start %d - end %d\n", firstSlot, firstSlot+EpochLength)

	for i := firstSlot; i < firstSlot+EpochLength && !failed.Load(); i++ {
		wg.Add(1)
		ch <- struct{}{}
		go func(slot int) {
			defer wg.Done()
			defer func() { <-ch }()

			vrfInput, err := vrfInputForMode(slot, nonceB, mode)
			if err != nil {
				fail(slot, fmt.Errorf("building VRF input: %w", err))
				return
			}
			_, output, err := vrfSigner.Prove(vrfInput)
			if err != nil {
				fail(slot, fmt.Errorf("VRF prove: %w", err))
				return
			}
			eligible, err := elig.CheckVRFOutput(output)
			if err != nil {
				fail(slot, fmt.Errorf("checking VRF output: %w", err))
				return
			}
			if eligible {
				slotsCh <- &assignedSlot{
					Slot:        slot,
					SlotInEpoch: int(AbsSlotToSlotInEpoch(Slot(slot))),
					Epoch:       e,
					Time:        AbsSlotToTime(Slot(slot)).In(tzLoc).Format(time.RFC3339),
				}
			}
		}(i)
	}
	wg.Wait()
	close(slotsCh)
	<-collectorDone

	if failed.Load() {
		return <-errCh
	}

	sort.SliceStable(ls.AssignedSlots, func(i, j int) bool { return ls.AssignedSlots[i].Slot < ls.AssignedSlots[j].Slot })

	if ideal > 0 {
		ls.Performance = float64(len(ls.AssignedSlots)) / ideal
	}

	summary, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling leader summary: %w", err)
	}
	fmt.Println(string(summary))
	return nil
}

// vrfInputForMode builds the VRF message exactly as gouroboros's leader
// election does: CPraos uses MkInputVrf, TPraos uses MkSeedTPraos with SeedL.
func vrfInputForMode(slot int, nonce []byte, mode consensus.ConsensusMode) ([]byte, error) {
	switch mode {
	case consensus.ConsensusModeTPraos:
		return vrf.MkSeedTPraos(int64(slot), nonce, vrf.SeedL()) //nolint:gosec
	default:
		return vrf.MkInputVrf(int64(slot), nonce)
	}
}

func consensusModeName(mode consensus.ConsensusMode) string {
	if mode == consensus.ConsensusModeTPraos {
		return "tpraos"
	}
	return "cpraos"
}

// main runs the tool and maps any error to a non-zero exit status, so that
// scripts can tell success from failure. Error output goes to stderr.
func main() {
	parseFlags()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("loading time zone %q: %w", tz, err)
	}
	tzLoc = loc

	mode, err := parseMode(consensusModeStr)
	if err != nil {
		return err
	}
	f, err := parseRat(fStr)
	if err != nil {
		return err
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	ds, err := newDataSource(ctx)
	if err != nil {
		return err
	}
	defer ds.Close()

	// A VRF signer is only needed when we actually evaluate slots, so we build
	// it lazily (only-nonce mode has no key).
	newSigner := func() (consensus.VRFSigner, error) {
		seed, err := vrfSeedFromCborHex(vrfSKeyCborHex)
		if err != nil {
			return nil, err
		}
		return consensus.NewSimpleVRFSigner(seed)
	}

	if epochNo > 0 {
		if epochNo < int(FirstShelleyEpoch) {
			return fmt.Errorf(
				"epoch %d is pre-Shelley (Byron); leader logs are only available from epoch %d on",
				epochNo, FirstShelleyEpoch)
		}
		info, err := ds.GetEpochInfo(ctx, epochNo)
		if err != nil {
			return err
		}
		if onlyNonce {
			fmt.Println(info.Nonce)
			return nil
		}
		poolStake, err := ds.GetPoolStakeForEpoch(ctx, epochNo, poolIdBech32)
		if err != nil {
			return err
		}
		signer, err := newSigner()
		if err != nil {
			return err
		}
		return checkSlotsInEpoch(
			epochNo,
			int(GetFirstSlotOfEpoch(Epoch(epochNo))),
			poolStake, info.ActiveStake, info.Nonce, f, mode, signer,
		)
	}

	var eta0 string
	if all || next {
		if CurrentSlotInEpoch() < SlotInEpochForNextNonce {
			fmt.Println("Next epoch nonce not yet computable")
		} else {
			var err error
			eta0, err = ds.GetCandidateNonce(ctx)
			if err != nil {
				return err
			}
			if onlyNonce {
				fmt.Println(eta0)
				return nil
			}
		}
	}

	if onlyNonce && poolIdBech32 == "" {
		if all || prev {
			info, err := ds.GetEpochInfo(ctx, int(CurrentEpoch())-1)
			if err != nil {
				return err
			}
			fmt.Println(info.Nonce)
		}
		if all || current {
			info, err := ds.GetEpochInfo(ctx, int(CurrentEpoch()))
			if err != nil {
				return err
			}
			fmt.Println(info.Nonce)
		}
		return nil
	}

	signer, err := newSigner()
	if err != nil {
		return err
	}

	snaps, err := ds.GetStakeSnapshots(ctx, poolIdBech32)
	if err != nil {
		return fmt.Errorf("fetching stake snapshots: %w", err)
	}
	// snapFor returns the named Go/Set/Mark snapshot, erroring instead of
	// feeding a zero-value (or missing) snapshot into the leader check.
	snapFor := func(kind string) (PoolStakeSnapshot, error) {
		snap, ok := snaps[kind]
		if !ok {
			return snap, fmt.Errorf("data source %q does not provide the %s stake snapshot for pool %s", ds.Name(), kind, poolIdBech32)
		}
		return snap, nil
	}

	if all || prev {
		snap, err := snapFor("Go")
		if err != nil {
			return err
		}
		nonce := snap.Nonce
		if onlyNonce {
			fmt.Println(nonce)
			return nil
		}
		if err := checkSlotsInEpoch(
			snap.EpochNo,
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			snap.PoolStake, snap.ActiveStake,
			nonce, f, mode, signer,
		); err != nil {
			return err
		}
	}

	if all || current {
		snap, err := snapFor("Set")
		if err != nil {
			return err
		}
		nonce := snap.Nonce
		if onlyNonce {
			fmt.Println(nonce)
			return nil
		}
		if err := checkSlotsInEpoch(
			snap.EpochNo,
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			snap.PoolStake, snap.ActiveStake,
			nonce, f, mode, signer,
		); err != nil {
			return err
		}
	}

	if (all || next) && CurrentSlotInEpoch() >= SlotInEpochForNextNonce {
		nonce := eta0
		if onlyNonce {
			fmt.Println(nonce)
			return nil
		}
		snap, err := snapFor("Mark")
		if err != nil {
			return err
		}
		if nonce == "" {
			return fmt.Errorf("next-epoch nonce not yet available from data source %q", ds.Name())
		}
		if err := checkSlotsInEpoch(
			snap.EpochNo,
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			snap.PoolStake, snap.ActiveStake,
			nonce, f, mode, signer,
		); err != nil {
			return err
		}
	}
	return nil
}
