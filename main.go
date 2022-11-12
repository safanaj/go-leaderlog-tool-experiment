package main

//go:generate go get github.com/otiai10/copy
//go:generate go run ./build_libsodium_helper.go _c_libsodium_built
//go:generate go mod tidy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"sort"
	"sync"
	"time"

	flag "github.com/spf13/pflag"

	koios "github.com/cardano-community/koios-go-client/v2"
	"github.com/echovl/cardano-go"
	cCli "github.com/echovl/cardano-go/cardano-cli"
)

var (
	vrfSKeyCborHex, poolIdBech32, tz    string
	onlyNonce, all, prev, current, next bool
	parallelFactor, epochNo             int

	tzLoc *time.Location
)

const activeSlotCoeff = 0.05

func parseFlags() {
	flag.StringVar(&vrfSKeyCborHex, "vrf-skey-cborhex", vrfSKeyCborHex, "VRF CBOR HEX")
	flag.StringVar(&poolIdBech32, "pool-id-bech32", poolIdBech32, "Pool id as bech32")
	flag.StringVar(&tz, "tz", "Local", "Time zone for the output")
	flag.BoolVar(&onlyNonce, "only-nonce", false, "just get nonce")
	flag.BoolVar(&all, "all", false, "compute for all epochs")
	flag.BoolVar(&prev, "prev", false, "compute for previous epoch")
	flag.BoolVar(&current, "current", true, "compute for current epoch")
	flag.BoolVar(&next, "next", false, "compute for next epoch")
	flag.IntVar(&parallelFactor, "parallel-factor", 30, "coefficient for cpu available")
	flag.IntVar(&epochNo, "epoch-no", epochNo, "compute for epoch (a past one)")
	flag.Parse()

	if (next || prev) && !all {
		current = false
	}
}

func getSigma(stakeRatio *big.Float) *big.Float {
	var sigma, nratio big.Float
	c := big.NewFloat(math.Log(1.0 - activeSlotCoeff))
	sigma.SetPrec(34).Mul(nratio.Neg(stakeRatio), c)
	sigmaF, acc := sigma.Float64()
	if acc != big.Exact {
		fmt.Printf("Not accurate %v: %v - %v\n", sigmaF, sigma, acc)
		return nil
	}
	return big.NewFloat(math.Exp(sigmaF))
}

// type leaderDetails struct {
// 	// "status": "ok",
// 	// "epoch": 374,
// 	// "epochNonce": "53606952e39eadd5eea559be517f9741c9538073e987ec1b7a6c7a05db6195d3",
// 	// "consensus": "praos",
// 	// "epochSlots": 0,
// 	// "epochSlotsIdeal": 0.07,
// 	// "maxPerformance": 0.0,
// 	// "poolId": "040d79b06f8ad1a94bd8fbc9eac9ba8ccf44a332d9d7f003d85f9393",
// 	// "sigma": 3.186425272454952e-6,
// 	// "activeStake": 79972214482,
// 	// "totalActiveStake": 25097784395987464,
// 	// "d": 0.0,
// 	// "f": 0.05,
// 	// "assignedSlots": []
// 	Epoch, Slot, SlotInEpoch     int
// 	PoolStake, TotalStake, Nonce string
// 	StakeRatio, Sigma, T, Ideal  string
// 	F                            float64
// }

type assignedSlot struct {
	Epoch, Slot, SlotInEpoch int
	Time                     string
}

type leaderSummary struct {
	Epoch                    int
	PoolStake, TotalStake    string
	Nonce, StakeRatio, Sigma string
	Ideal, Performance       string
	F                        float64
	AssignedSlots            []*assignedSlot
}

func checkSlotsInEpoch(e, firstSlot int, pStake, aStake *big.Float, eta0, vrfCborHex string) {
	var wg sync.WaitGroup
	concurrency := runtime.NumCPU() * parallelFactor
	if concurrency == 0 {
		concurrency = 1
	}
	ch := make(chan struct{}, concurrency)
	slotsCh := make(chan *assignedSlot)

	// epoch common values
	var stakeRatio, ideal, perf big.Float
	certMax := getVrfMaxValue()
	sigma := getSigma(stakeRatio.Quo(pStake, aStake))
	ideal.Mul(&stakeRatio, big.NewFloat(EpochLength*activeSlotCoeff)).SetPrec(2)
	ls := &leaderSummary{
		Epoch:      e,
		Nonce:      eta0,
		PoolStake:  pStake.String(),
		TotalStake: aStake.String(),
		StakeRatio: stakeRatio.String(),
		Sigma:      sigma.String(),
		F:          activeSlotCoeff,
		Ideal:      ideal.String(),
	}

	// assigned slots collector
	go func() {
		for {
			as, notDone := <-slotsCh
			if !notDone {
				return
			}
			ls.AssignedSlots = append(ls.AssignedSlots, as)
		}
	}()

	for i := firstSlot; i < firstSlot+EpochLength; i++ {
		wg.Add(1)
		ch <- struct{}{}
		go func(slot int) {
			certLeaderVrf := getVrfLeaderValue(slot, eta0, vrfCborHex)
			den := big.NewFloat(0).SetPrec(34).SetInt(big.NewInt(0).Sub(certMax, certLeaderVrf))
			num := big.NewFloat(0).SetPrec(34).SetInt(certMax)
			q := big.NewFloat(0).SetPrec(34).Quo(num, den)
			if q.Cmp(sigma) < 1 {
				slotsCh <- &assignedSlot{
					Slot:        slot,
					SlotInEpoch: slot % EpochLength,
					Epoch:       e,
					Time:        AbsSlotToTime(Slot(slot)).In(tzLoc).Format(time.RFC3339),
				}
			}
			<-ch
			wg.Done()
		}(i)
	}
	wg.Wait()
	close(ch)
	close(slotsCh)
	sort.SliceStable(ls.AssignedSlots, func(i, j int) bool { return ls.AssignedSlots[i].Slot < ls.AssignedSlots[j].Slot })

	perf.Quo(
		perf.Quo(
			big.NewFloat(float64(len(ls.AssignedSlots))),
			big.NewFloat(0).Mul(&ideal, big.NewFloat(10000.0))).SetPrec(2),
		big.NewFloat(100.0)).SetPrec(2)
	ls.Performance = perf.String()

	summary, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		fmt.Println(err)
	}
	fmt.Println(string(summary))
}

func main() {
	parseFlags()
	if tzLoc_, err := time.LoadLocation(tz); err != nil {
		panic(err)
	} else {
		tzLoc = tzLoc_
	}

	ccli := cCli.NewNode(cardano.Mainnet).(*cCli.CardanoCli)
	ctx, ctxCancel := context.WithCancel(context.Background())
	kc, _ := koios.New()

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

		checkSlotsInEpoch(
			int(epochNo),
			int(GetFirstSlotOfEpoch(Epoch(epochNo))),
			poolStake.BigFloat(),
			activeStake.BigFloat(),
			nonce, vrfSKeyCborHex)
		return
	}

	var eta0, nonce string
	if all || next {
		if CurrentSlotInEpoch() < SlotInEpochForNextNonce {
			fmt.Println("Next epoch nonce not yet computable")
		} else {
			eta0 = getEta0(ccli)
			if onlyNonce {
				fmt.Println(eta0)
				return
			}
		}
	}

	if initialize_libsodium() == -1 {
		panic("sodium_init() failed")
	}

	snaps := getStakeSnapshotForPool(ctx, kc, poolIdBech32)
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

	if all || prev {
		snap := snaps["Go"]
		nonce = snap.Nonce
		if onlyNonce {
			fmt.Println(nonce)
			return
		}
		checkSlotsInEpoch(
			int(snap.EpochNo),
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			snap.PoolStake.BigFloat(), snap.ActiveStake.BigFloat(),
			nonce, vrfSKeyCborHex)
	}

	if all || current {
		snap := snaps["Set"]
		nonce = snap.Nonce
		if onlyNonce {
			fmt.Println(nonce)
			return
		}
		checkSlotsInEpoch(
			int(snap.EpochNo),
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			snap.PoolStake.BigFloat(), snap.ActiveStake.BigFloat(),
			nonce, vrfSKeyCborHex)
	}

	if (all || next) && CurrentSlotInEpoch() >= SlotInEpochForNextNonce {
		nonce = eta0
		if onlyNonce {
			fmt.Println(nonce)
			return
		}
		snap := snaps["Mark"]
		checkSlotsInEpoch(
			int(snap.EpochNo),
			int(GetFirstSlotOfEpoch(Epoch(snap.EpochNo))),
			snap.PoolStake.BigFloat(), snap.ActiveStake.BigFloat(),
			nonce, vrfSKeyCborHex)
	}

	ctxCancel()
	<-ctx.Done()
}
