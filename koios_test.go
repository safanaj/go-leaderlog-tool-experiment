package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeNonce is a valid 64-hex-char (32-byte) epoch nonce.
const fakeNonce = "1dc408ac6e6d230e2560a288943ede3b952585e2f1cfb0312afa9703cdb3f2ee"

// koiosFixture serves the Koios endpoints the tool uses (raw epoch_info /
// epoch_params and the typed pool_stake_snapshot / pool_history), so the
// koiosDataSource can be tested end-to-end without network access.
type koiosFixture struct {
	srv *httptest.Server
}

func newKoiosFixture(t *testing.T) *koiosFixture {
	t.Helper()
	f := &koiosFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// strip an optional /api/<version> prefix so the handler sees the
		// bare endpoint name (the client resolves paths against its base URL)
		segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(segs) >= 3 && segs[0] == "api" {
			segs = segs[2:]
		}
		endpoint := "/" + strings.Join(segs, "/")

		q := r.URL.Query()
		epoch := q.Get("_epoch_no")
		switch endpoint {
		case "/epoch_info":
			switch epoch {
			case "550":
				fmt.Fprint(w, `[{"active_stake":"34000000000000000"}]`)
			case "551":
				fmt.Fprint(w, `[{"active_stake":"1"}]`)
			default:
				http.NotFound(w, r)
			}
		case "/epoch_params":
			switch epoch {
			case "550":
				fmt.Fprintf(w, `[{"block_hash":"2c7bc4b8","nonce":"%s"}]`, fakeNonce)
			default:
				// epoch 551: no nonce row
				fmt.Fprint(w, `[]`)
			}
		case "/pool_stake_snapshot":
			fmt.Fprintf(w, `[
				{"snapshot":"Go","epoch_no":549,"nonce":"%s","pool_stake":"1000000","active_stake":"33000000000000000"},
				{"snapshot":"Set","epoch_no":550,"nonce":"%s","pool_stake":"2000000","active_stake":"34000000000000000"},
				{"snapshot":"Mark","epoch_no":551,"nonce":"%s","pool_stake":"3000000","active_stake":"35000000000000000"}
			]`, fakeNonce, fakeNonce, fakeNonce)
		case "/pool_history":
			if q.Get("_pool_bech32") == "pool1xyz" && epoch == "550" {
				fmt.Fprint(w, `[{"active_stake":"2000000"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestKoiosDataSource exercises the koios DataSource against a local fixture:
// the raw HTTP epoch reads (the "order" workaround path) and the typed client
// reads (pool history / snapshots), all routed through KOIOS_API_URL.
func TestKoiosDataSource(t *testing.T) {
	f := newKoiosFixture(t)
	t.Setenv("KOIOS_API_URL", f.srv.URL+"/api/v1")

	ctx := context.Background()
	ds, err := NewKoiosDataSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	if ds.Name() != "koios" {
		t.Errorf("Name() = %q, want koios", ds.Name())
	}

	t.Run("GetEpochInfo", func(t *testing.T) {
		info, err := ds.GetEpochInfo(ctx, 550)
		if err != nil {
			t.Fatal(err)
		}
		if info.Nonce != fakeNonce {
			t.Errorf("nonce = %q, want %q", info.Nonce, fakeNonce)
		}
		if info.ActiveStake != 34_000_000_000_000_000 {
			t.Errorf("active stake = %d, want 34000000000000000", info.ActiveStake)
		}
		if info.BlockHash != "2c7bc4b8" {
			t.Errorf("block hash = %q, want 2c7bc4b8", info.BlockHash)
		}
	})

	t.Run("GetEpochInfo unknown epoch", func(t *testing.T) {
		// epoch 551 has an epoch_info row but no epoch_params row.
		if _, err := ds.GetEpochInfo(ctx, 551); err == nil || !strings.Contains(err.Error(), "no nonce for epoch 551") {
			t.Errorf("want no-nonce error, got: %v", err)
		}
	})

	t.Run("GetPoolStakeForEpoch", func(t *testing.T) {
		stake, err := ds.GetPoolStakeForEpoch(ctx, 550, "pool1xyz")
		if err != nil {
			t.Fatal(err)
		}
		if stake != 2_000_000 {
			t.Errorf("pool stake = %d, want 2000000", stake)
		}
		// unknown pool in a known epoch must be a clear error, not a panic
		if _, err := ds.GetPoolStakeForEpoch(ctx, 550, "pool1unknown"); err == nil || !strings.Contains(err.Error(), "no pool history") {
			t.Errorf("want no-pool-history error, got: %v", err)
		}
	})

	t.Run("GetStakeSnapshots", func(t *testing.T) {
		snaps, err := ds.GetStakeSnapshots(ctx, "pool1xyz")
		if err != nil {
			t.Fatal(err)
		}
		set, ok := snaps["Set"]
		if !ok {
			t.Fatal("want Set snapshot")
		}
		if set.EpochNo != 550 || set.PoolStake != 2_000_000 || set.ActiveStake != 34_000_000_000_000_000 || set.Nonce != fakeNonce {
			t.Errorf("Set snapshot = %+v", set)
		}
		prevSnap, ok := snaps["Go"]
		if !ok || prevSnap.EpochNo != 549 || prevSnap.PoolStake != 1_000_000 {
			t.Errorf("Go snapshot = %+v (ok=%v)", prevSnap, ok)
		}
		mark, ok := snaps["Mark"]
		if !ok || mark.EpochNo != 551 || mark.PoolStake != 3_000_000 {
			t.Errorf("Mark snapshot = %+v (ok=%v)", mark, ok)
		}
	})
}

// TestKoiosOptionsFromEnv pins the KOIOS_API_URL → koios client option
// mapping (so the typed client and the raw HTTP path agree on the host).
func TestKoiosOptionsFromEnv(t *testing.T) {
	t.Setenv("KOIOS_API_URL", "https://api.koios.io/api/v1")
	opts, err := koiosOptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 3 { // scheme, host, api version (default port)
		t.Errorf("got %d options, want 3", len(opts))
	}

	t.Setenv("KOIOS_API_URL", "http://localhost:8050/api/v0")
	opts, err = koiosOptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 4 { // + port
		t.Errorf("got %d options, want 4", len(opts))
	}

	for _, bad := range []string{"ftp://api.koios.io/api/v1", "https://api.koios.io/other/v1", "https:///api/v1"} {
		t.Setenv("KOIOS_API_URL", bad)
		if _, err := koiosOptionsFromEnv(); err == nil {
			t.Errorf("koiosOptionsFromEnv(%q) should fail", bad)
		}
	}

	t.Setenv("KOIOS_API_URL", "")
	if opts, err := koiosOptionsFromEnv(); err != nil || opts != nil {
		t.Errorf("unset KOIOS_API_URL should yield no options and no error, got %v, %v", opts, err)
	}
}

// TestParseNetwork pins the -node-network mapping.
func TestParseNetwork(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", "mainnet"},
		{"mainnet", "mainnet"},
		{"PreProd", "preprod"},
		{"preproduction", "preprod"},
		{"testnet", "preprod"}, // legacy alias
		{"preview", "preview"},
	} {
		network, err := parseNetwork(tc.in)
		if err != nil {
			t.Fatalf("parseNetwork(%q): %v", tc.in, err)
		}
		if network.Name != tc.want {
			t.Errorf("parseNetwork(%q).Name = %q, want %q", tc.in, network.Name, tc.want)
		}
	}
	if _, err := parseNetwork("bogus"); err == nil {
		t.Error("parseNetwork(bogus) should fail")
	}
}
