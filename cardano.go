package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	koios "github.com/cardano-community/koios-go-client/v4"
	"github.com/shopspring/decimal"
)

// getEpochInfoForNonce fetches the active stake and epoch nonce for a given
// epoch from Koios. The nonce is the hex-encoded 32-byte epoch nonce (eta0).
//
// These two reads are done with a raw HTTP GET (rather than the koios-go-client
// v4 GetEpochInfo/GetEpochParams helpers) because those helpers unconditionally
// inject an "order=epoch_no.desc" query parameter, which the Koios v1 REST API
// rejects when combined with an epoch filter ("column record.epoch_no does not
// exist"). We reuse the client's own base URL so the host/version stay
// consistent, and allow KOIOS_API_URL to override it.
func getEpochInfoForNonce(ctx context.Context, kc *koios.Client, epochNo Epoch) (uint64, string, string, error) {
	base := koiosBaseURL(kc)
	ep := strconv.FormatInt(int64(epochNo), 10)

	var activeStake uint64
	{
		var out []struct {
			ActiveStake string `json:"active_stake"`
		}
		if err := koiosGetJSON(ctx, base, "epoch_info", url.Values{
			"select":    []string{"active_stake"},
			"_epoch_no": []string{ep},
		}, &out); err != nil {
			return 0, "", "", err
		}
		if len(out) == 0 {
			return 0, "", "", fmt.Errorf("no active_stake for epoch %d", epochNo)
		}
		stake, err := stakeUint64FromStr(out[0].ActiveStake)
		if err != nil {
			return 0, "", "", fmt.Errorf("active_stake for epoch %d: %w", epochNo, err)
		}
		activeStake = stake
	}

	var blockHash, nonce string
	{
		var out []struct {
			BlockHash string `json:"block_hash"`
			Nonce     string `json:"nonce"`
		}
		if err := koiosGetJSON(ctx, base, "epoch_params", url.Values{
			"select":    []string{"block_hash,nonce"},
			"_epoch_no": []string{ep},
		}, &out); err != nil {
			return 0, "", "", err
		}
		if len(out) == 0 {
			return 0, "", "", fmt.Errorf("no nonce for epoch %d", epochNo)
		}
		blockHash = out[0].BlockHash
		nonce = out[0].Nonce
	}
	return activeStake, blockHash, nonce, nil
}

// koiosBaseURL returns the Koios REST base URL, defaulting to the configured
// client but allowing an override via KOIOS_API_URL (e.g. the mainnet host
// https://api.koios.io/api/v1/).
func koiosBaseURL(kc *koios.Client) string {
	if u := os.Getenv("KOIOS_API_URL"); u != "" {
		return strings.TrimRight(u, "/") + "/"
	}
	return strings.TrimRight(kc.BaseURL(), "/") + "/"
}

// koiosGetJSON performs a raw GET against <base><path> with the given query and
// decodes a JSON array into out.
func koiosGetJSON(ctx context.Context, base, path string, query url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.URL.RawQuery = query.Encode()

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("koios %s: HTTP %d: %s", path, res.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func getPoolStakeForEpoch(ctx context.Context, kc *koios.Client, epochNo int, id string) (uint64, error) {
	epochNo_ := koios.EpochNo(epochNo)
	opts := kc.NewRequestOptions()
	opts.QuerySet("select", "active_stake")
	r, err := kc.GetPoolHistory(ctx, koios.PoolID(id), epochNo_, opts)
	if err != nil {
		return 0, err
	}
	return stakeUint64(r.Data[0].ActiveStake)
}

func getStakeSnapshotForPool(ctx context.Context, kc *koios.Client, id string) map[string]koios.PoolSnapshot {
	r, _ := kc.GetPoolStakeSnapshot(ctx, koios.PoolID(id), nil)
	m := make(map[string]koios.PoolSnapshot)
	for _, s := range r.Data {
		m[s.Snapshot] = s
	}
	return m
}

// getEta0 returns the candidate nonce (eta0) for the upcoming epoch.
//
// The cardano-node CLI query used to be supported here, but it required the
// cardano-go dependency and a running node. It is now obtained from a public
// HTTP endpoint; when that endpoint has not yet produced a nonce, an empty
// string is returned.
func getEta0() string {
	if os.Getenv("CARDANO_NODE_SOCKET_PATH") == "" {
		return eta0FromHTTP()
	}
	// A node is configured but we no longer query it; fall back to HTTP so the
	// behaviour is well-defined rather than failing.
	return eta0FromHTTP()
}

func eta0FromHTTP() string {
	res, err := http.Get("https://f2lb.bardels.me/api/v0/nonce.next")
	if err != nil {
		panic(err)
	}
	if res.StatusCode == http.StatusNotFound {
		return ""
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(b))
}

// nonceBytes decodes a hex-encoded 32-byte epoch nonce, tolerating a trailing
// newline (some Koios responses include one).
func nonceBytes(hexNonce string) ([32]byte, error) {
	hexNonce = strings.TrimSpace(hexNonce)
	b, err := hex.DecodeString(hexNonce)
	if err != nil {
		return [32]byte{}, fmt.Errorf("invalid nonce hex %q: %w", hexNonce, err)
	}
	if len(b) != 32 {
		return [32]byte{}, fmt.Errorf("nonce must be 32 bytes, got %d", len(b))
	}
	var out [32]byte
	copy(out[:], b)
	return out, nil
}

// nonceHex renders a 32-byte epoch nonce as lowercase hex.
func nonceHex(n [32]byte) string {
	return hex.EncodeToString(n[:])
}

// uint64FromSnap converts a Koios PoolSnapshot decimal stake field to a uint64
// in lovelace, defaulting to zero if the field is missing/empty.
func uint64FromSnap(d decimal.Decimal) uint64 {
	if !d.IsPositive() {
		return 0
	}
	bi := d.BigInt()
	if bi == nil || !bi.IsUint64() {
		return 0
	}
	return bi.Uint64()
}

// stakeUint64 converts a Koios decimal active-stake value to a uint64 in
// lovelace. Cardano total active stake is well within uint64 range.
func stakeUint64(d decimal.Decimal) (uint64, error) {
	bi := d.BigInt()
	if bi == nil || bi.Sign() < 0 {
		return 0, fmt.Errorf("invalid stake %s", d.String())
	}
	if !bi.IsUint64() {
		return 0, fmt.Errorf("stake %s does not fit in uint64", d.String())
	}
	return bi.Uint64(), nil
}

// stakeUint64FromStr parses a Koios active-stake string (lovelace) to a uint64.
func stakeUint64FromStr(s string) (uint64, error) {
	bi, ok := new(big.Int).SetString(strings.TrimSpace(s), 10)
	if !ok {
		return 0, fmt.Errorf("invalid stake string %q", s)
	}
	if bi.Sign() < 0 || !bi.IsUint64() {
		return 0, fmt.Errorf("stake %s does not fit in uint64", s)
	}
	return bi.Uint64(), nil
}
