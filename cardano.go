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
	"time"

	koios "github.com/cardano-community/koios-go-client/v4"
	"github.com/shopspring/decimal"
)

// koiosDataSource is the default DataSource: the Koios REST API (v4 client).
//
// It exists to keep the consensus core free of data-access concerns: it feeds
// checkSlotsInEpoch the epoch nonce and the (per-pool / total) active stake,
// exactly as the free functions it replaced did.
//
// The KOIOS_API_URL environment variable overrides the host for *all* reads
// (raw and typed), e.g. https://api.koios.io/api/v1.
type koiosDataSource struct {
	kc *koios.Client
}

var _ DataSource = (*koiosDataSource)(nil)

// NewKoiosDataSource builds the koios client, applying the KOIOS_API_URL
// override (if set) as client options so that the typed client and the raw
// HTTP path below always agree on the host.
func NewKoiosDataSource(ctx context.Context) (DataSource, error) {
	opts, err := koiosOptionsFromEnv()
	if err != nil {
		return nil, err
	}
	kc, err := koios.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating koios client: %w", err)
	}
	return &koiosDataSource{kc: kc}, nil
}

func (k *koiosDataSource) Name() string { return "koios" }

func (k *koiosDataSource) Close() error { return nil }

// koiosOptionsFromEnv turns KOIOS_API_URL (expected shape:
// scheme://host[:port]/api/<version>) into koios client options.
func koiosOptionsFromEnv() ([]koios.Option, error) {
	raw := strings.TrimSpace(os.Getenv("KOIOS_API_URL"))
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing KOIOS_API_URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("KOIOS_API_URL %q: unsupported scheme %q", raw, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("KOIOS_API_URL %q: missing host", raw)
	}
	var version string
	if path := strings.Trim(u.Path, "/"); path != "" {
		parts := strings.Split(path, "/")
		if len(parts) != 2 || parts[0] != "api" {
			return nil, fmt.Errorf("KOIOS_API_URL %q: path must be /api/<version>, got %q", raw, u.Path)
		}
		version = parts[1]
	}
	opts := []koios.Option{koios.Scheme(u.Scheme), koios.Host(host)}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("KOIOS_API_URL %q: invalid port %q", raw, port)
		}
		opts = append(opts, koios.Port(uint16(n)))
	}
	if version != "" {
		opts = append(opts, koios.APIVersion(version))
	}
	return opts, nil
}

// GetEpochInfo fetches the active stake and epoch nonce for a given epoch
// from Koios. The nonce is the hex-encoded 32-byte epoch nonce (eta0).
//
// These two reads are done with a raw HTTP GET (rather than the koios-go-client
// v4 GetEpochInfo/GetEpochParams helpers) because those helpers unconditionally
// inject an "order=epoch_no.desc" query parameter, which the Koios v1 REST API
// rejects when combined with an epoch filter ("column record.epoch_no does not
// exist"). We reuse the client's own base URL so the host/version stay
// consistent. Do not route these reads back through GetEpochInfo/GetEpochParams
// — it will 400 again.
func (k *koiosDataSource) GetEpochInfo(ctx context.Context, epochNo int) (EpochInfo, error) {
	base := koiosBaseURL(k.kc)
	ep := strconv.FormatInt(int64(epochNo), 10)

	var info EpochInfo
	{
		var out []struct {
			ActiveStake string `json:"active_stake"`
		}
		if err := koiosGetJSON(ctx, base, "epoch_info", url.Values{
			"select":    []string{"active_stake"},
			"_epoch_no": []string{ep},
		}, &out); err != nil {
			return info, err
		}
		if len(out) == 0 {
			return info, fmt.Errorf("no active_stake for epoch %d", epochNo)
		}
		stake, err := stakeUint64FromStr(out[0].ActiveStake)
		if err != nil {
			return info, fmt.Errorf("active_stake for epoch %d: %w", epochNo, err)
		}
		info.ActiveStake = stake
	}

	{
		var out []struct {
			BlockHash string `json:"block_hash"`
			Nonce     string `json:"nonce"`
		}
		if err := koiosGetJSON(ctx, base, "epoch_params", url.Values{
			"select":    []string{"block_hash,nonce"},
			"_epoch_no": []string{ep},
		}, &out); err != nil {
			return info, err
		}
		if len(out) == 0 {
			return info, fmt.Errorf("no nonce for epoch %d", epochNo)
		}
		info.BlockHash = out[0].BlockHash
		info.Nonce = out[0].Nonce
	}
	return info, nil
}

func (k *koiosDataSource) GetPoolStakeForEpoch(ctx context.Context, epochNo int, id string) (uint64, error) {
	epochNo_ := koios.EpochNo(epochNo)
	opts := k.kc.NewRequestOptions()
	opts.QuerySet("select", "active_stake")
	r, err := k.kc.GetPoolHistory(ctx, koios.PoolID(id), epochNo_, opts)
	if err != nil {
		return 0, fmt.Errorf("koios pool_history for epoch %d: %w", epochNo, err)
	}
	if len(r.Data) == 0 {
		// Unknown pool id, or a pool with no history in that epoch.
		return 0, fmt.Errorf("no pool history for pool %s in epoch %d", id, epochNo)
	}
	return stakeUint64(r.Data[0].ActiveStake)
}

func (k *koiosDataSource) GetStakeSnapshots(ctx context.Context, id string) (map[string]PoolStakeSnapshot, error) {
	r, err := k.kc.GetPoolStakeSnapshot(ctx, koios.PoolID(id), nil)
	if err != nil {
		return nil, fmt.Errorf("koios pool_stake_snapshot for pool %s: %w", id, err)
	}
	if len(r.Data) == 0 {
		return nil, fmt.Errorf("no stake snapshots for pool %s (unknown pool id?)", id)
	}
	m := make(map[string]PoolStakeSnapshot, len(r.Data))
	for _, s := range r.Data {
		m[s.Snapshot] = PoolStakeSnapshot{
			EpochNo:     int(s.EpochNo),
			ActiveStake: uint64FromSnap(s.ActiveStake),
			PoolStake:   uint64FromSnap(s.PoolStake),
			Nonce:       s.Nonce,
		}
	}
	return m, nil
}

// GetCandidateNonce returns the candidate nonce (eta0) for the upcoming epoch
// from the public f2lb HTTP endpoint, which computes it from the evolving
// nonce (the Koios API itself does not expose it).
func (k *koiosDataSource) GetCandidateNonce(ctx context.Context) (string, error) {
	return eta0FromHTTP(ctx)
}

// koiosBaseURL returns the Koios REST base URL of the configured client.
func koiosBaseURL(kc *koios.Client) string {
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

// eta0URL is the public endpoint serving the candidate nonce (eta0) for the
// upcoming epoch. It is a variable (not a constant) so tests can point it at
// a local httptest server.
var eta0URL = "https://f2lb.bardels.me/api/v0/nonce.next"

// eta0Timeout bounds a single eta0 fetch.
var eta0Timeout = 15 * time.Second

// eta0FromHTTP returns the candidate nonce (eta0) for the upcoming epoch.
// When the endpoint has not yet produced a nonce for the upcoming epoch an
// empty string is returned (that is not an error).
func eta0FromHTTP(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, eta0Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eta0URL, nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", eta0URL, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		// The nonce for the upcoming epoch has not been published yet.
		return "", nil
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("%s: HTTP %d: %s", eta0URL, res.StatusCode, strings.TrimSpace(string(body)))
	}
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s response: %w", eta0URL, err)
	}
	return strings.TrimSpace(string(b)), nil
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
