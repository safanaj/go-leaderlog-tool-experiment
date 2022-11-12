package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"

	koios "github.com/cardano-community/koios-go-client/v2"
	cCli "github.com/echovl/cardano-go/cardano-cli"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/blake2b"
)

func getEpochInfoForNonce(ctx context.Context, kc *koios.Client, epochNo Epoch) (decimal.Decimal, string, string, error) {
	var activeStake decimal.Decimal
	var blockHash, nonce string

	epoch := koios.EpochNo(epochNo)
	{
		opts := kc.NewRequestOptions()
		opts.QuerySet("select", "active_stake")
		r, err := kc.GetEpochInfo(ctx, &epoch, opts)
		if err != nil {
			return decimal.Zero, "", "", err
		}
		activeStake = r.Data[0].ActiveStake
	}

	{
		opts := kc.NewRequestOptions()
		opts.QuerySet("select", "block_hash,nonce")
		r, _ := kc.GetEpochParams(ctx, &epoch, opts)
		blockHash = string(r.Data[0].BlockHash)
		nonce = r.Data[0].Nonce
	}
	return activeStake, blockHash, nonce, nil
}

func getPoolStakeForEpoch(ctx context.Context, kc *koios.Client, epochNo int, id string) (decimal.Decimal, error) {
	epochNo_ := koios.EpochNo(epochNo)
	opts := kc.NewRequestOptions()
	opts.QuerySet("select", "active_stake")
	r, err := kc.GetPoolHistory(ctx, koios.PoolID(id), &epochNo_, opts)
	if err != nil {
		return decimal.Zero, err
	}
	return r.Data[0].ActiveStake, nil
}

func getStakeSnapshotForPool(ctx context.Context, kc *koios.Client, id string) map[string]koios.PoolSnapshot {
	r, _ := kc.GetPoolSnapshot(ctx, koios.PoolID(id), nil)
	m := make(map[string]koios.PoolSnapshot)
	for _, s := range r.Data {
		m[s.Snapshot] = s
	}
	return m
}

func getEta0(ccli *cCli.CardanoCli) string {
	if os.Getenv("CARDANO_NODE_SOCKET_PATH") == "" {
		// not possible to use a node, fallback on f2lb.bardels.me for the next nonce
		res, err := http.Get("https://f2lb.bardels.me/api/v0/nonce.next")
		if err != nil {
			panic(err)
		}
		if res.StatusCode == http.StatusNotFound {
			return ""
		}
		b, err := io.ReadAll(res.Body)
		if err != nil {
			panic(err)
		}
		return string(b)
	}

	out, err := ccli.DoCommand("query", "protocol-state")
	if err != nil {
		panic(err)
	}
	res := map[string]any{}
	err = json.Unmarshal([]byte(out), &res)
	if err != nil {
		panic(err)
	}

	cn := res["candidateNonce"].(map[string]any)["contents"].(string)
	lebn := res["lastEpochBlockNonce"].(map[string]any)["contents"].(string)

	eta0, _ := hex.DecodeString(cn + lebn)
	h := blake2b.Sum256(eta0)
	return hex.EncodeToString(h[:])
}
