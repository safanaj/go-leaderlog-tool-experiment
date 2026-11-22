package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	flag "github.com/spf13/pflag"

	consensus "github.com/blinklabs-io/gouroboros/consensus"
	"github.com/blinklabs-io/gouroboros/vrf"
	koios "github.com/cardano-community/koios-go-client/v4"
)

var (
	vrfSKeyCborHex, poolIdBech32, tz    string
	onlyNonce, all, prev, current, next bool
	parallelFactor, epochNo             int
	consensusModeStr                    string
	fStr                                string

	tzLoc *time.Location
)

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

	flag.Parse()

	if (next || prev) && !all {
		current = false
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
func checkSlotsInEpoch(
	e, firstSlot int,
	poolStake, totalStake uint64,
	nonce string,
	f *big.Rat,
	mode consensus.ConsensusMode,
	vrfSigner consensus.VRFSigner,
) {
	nb, err := nonceBytes(nonce)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	nonceB := nb[:]

	elig, err := consensus.NewLeaderEligibility(poolStake, totalStake, f, mode)
	if err != nil {
		fmt.Printf("Error preparing leader eligibility: %v\n", err)
		return
	}

	var wg sync.WaitGroup
	concurrency := runtime.NumCPU() * parallelFactor
	if concurrency == 0 {
		concurrency = 1
	}
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

	// assigned slots collector
	go func() {
		for as := range slotsCh {
			ls.AssignedSlots = append(ls.AssignedSlots, as)
		}
	}()

	fmt.Printf("slots range: start %d - end %d\n", firstSlot, firstSlot+EpochLength)

	for i := firstSlot; i < firstSlot+EpochLength; i++ {
		wg.Add(1)
		ch <- struct{}{}
		go func(slot int) {
			defer wg.Done()
			defer func() { <-ch }()

			vrfInput, err := vrfInputForMode(slot, nonceB, mode)
			if err != nil {
				return
			}
			_, output, err := vrfSigner.Prove(vrfInput)
			if err != nil {
				return
			}
			eligible, err := elig.CheckVRFOutput(output)
			if err != nil {
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

	sort.SliceStable(ls.AssignedSlots, func(i, j int) bool { return ls.AssignedSlots[i].Slot < ls.AssignedSlots[j].Slot })

	if ideal > 0 {
		ls.Performance = float64(len(ls.AssignedSlots)) / ideal
	}

	summary, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(summary))
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

func main() {
	parseFlags()

	tzLoc_, err := time.LoadLocation(tz)
	if err != nil {
		panic(err)
	}
	tzLoc = tzLoc_

	mode, err := parseMode(consensusModeStr)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	f, err := parseRat(fStr)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	kc, err := koios.New()
	if err != nil {
		panic(err)
	}

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
		activeStake, _, nonce, err := getEpochInfoForNonce(ctx, kc, Epoch(epochNo))
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		if onlyNonce {
			fmt.Println(nonce)
			return
		}
		poolStake, err := getPoolStakeForEpoch(ctx, kc, epochNo, poolIdBech32)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		signer, err := newSigner()
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		checkSlotsInEpoch(
			int(epochNo),
			int(GetFirstSlotOfEpoch(Epoch(epochNo))),
			poolStake, activeStake, nonce, f, mode, signer,
		)
		return
	}

	var eta0 string
	if all || next {
		if CurrentSlotInEpoch() < SlotInEpochForNextNonce {
			fmt.Println("Next epoch nonce not yet computable")
		} else {
			eta0 = getEta0()
			if onlyNonce {
				fmt.Println(eta0)
				return
			}
		}
	}

	if onlyNonce && poolIdBech32 == "" {
		if all || prev {
			_, _, nonce, err := getEpochInfoForNonce(ctx, kc, CurrentEpoch()-1)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				return
			}
			fmt.Println(nonce)
		}
		if all || current {
			_, _, nonce, err := getEpochInfoForNonce(ctx, kc, CurrentEpoch())
			if err != nil {
				fmt.Printf("Error: %v\n", err)
				return
			}
			fmt.Println(nonce)
		}
		return
	}

	signer, err := newSigner()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	snaps := getStakeSnapshotForPool(ctx, kc, poolIdBech32)

	if all || prev {
		snap := snaps["Go"]
		nonce := snap.Nonce
		if onlyNonce {
			fmt.Println(nonce)
			return
		}
		checkSlotsInEpoch(
			int(snap.EpochNo),
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			uint64FromSnap(snap.PoolStake), uint64FromSnap(snap.ActiveStake),
			nonce, f, mode, signer,
		)
	}

	if all || current {
		snap := snaps["Set"]
		nonce := snap.Nonce
		if onlyNonce {
			fmt.Println(nonce)
			return
		}
		checkSlotsInEpoch(
			int(snap.EpochNo),
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			uint64FromSnap(snap.PoolStake), uint64FromSnap(snap.ActiveStake),
			nonce, f, mode, signer,
		)
	}

	if (all || next) && CurrentSlotInEpoch() >= SlotInEpochForNextNonce {
		nonce := eta0
		if onlyNonce {
			fmt.Println(nonce)
			return
		}
		snap := snaps["Mark"]
		checkSlotsInEpoch(
			int(snap.EpochNo),
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			uint64FromSnap(snap.PoolStake), uint64FromSnap(snap.ActiveStake),
			nonce, f, mode, signer,
		)
	}
}
