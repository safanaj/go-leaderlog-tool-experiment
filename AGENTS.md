# AGENTS.md

> A guide for AI agents (and humans) continuing work on **go-leaderlog-tool**.
> Read this first. It captures the *why* behind the current design, which is the
> part that is easy to get wrong.

## What this project is

A Go re-implementation of Cardano's PRAOS leader-log scheduler — the same thing
`cncli leaderlog` (Rust) and the Haskell `cardano-node` do. Given a pool's VRF
signing key, an epoch nonce, and the pool's/active stake, it enumerates every
slot of an epoch and reports which slots the pool would win as slot leader.

The correctness bar is: **its output must match `cncli` / `cardano-node` exactly**,
because that determines which blocks a pool actually produces on-chain.

## The core algorithm (and the part that bites)

For each slot `s` of an epoch with nonce `η0` (32 bytes):

1. Build the VRF message:
   - **CPraos** (Babbage+, the default): `vrf.MkInputVrf(s, η0)`
   - **TPraos** (Shelley–Alonzo): `vrf.MkSeedTPraos(s, η0, vrf.SeedL())`
2. `output = VRF.Prove(message)` using the pool's VRF key.
3. Decide leadership with the **finite E34 predicate** (a fixed-point `big.Int`
   scaled by `10^34`, floor division everywhere), i.e. `x = -(σ·c)` and "is a
   leader iff `q < x`" where `q` is derived from the VRF output.

The whole reason this repo was rewritten (see **History** below) is that a naive
floating-point / decimal implementation rounds **differently** from the
reference and produces wrong leader slots. The fix was to stop computing E34
ourselves and **delegate it to `gouroboros/consensus`**, which is the same E34
primitive Dingo's node uses.

### Do NOT reimplement the E34 math yourself.

E34 constants live in `gouroboros@v0.220.4/consensus/e34.go`:
- `e34Scale = 10^34`, `e34Epsilon = 10^10`
- all division is **floor toward −∞** (`e34FloorQuotient`), `ln`/`exp` are
  computed at this precision.

The leader test is `gouroboros/consensus`:
- `NewLeaderEligibility(poolStake, totalStake uint64, f *big.Rat, mode ConsensusMode)`
- `(*LeaderEligibility).CheckVRFOutput(output []byte) (bool, error)`
- one-shot equivalent: `IsSlotLeaderFromComponentsWithMode(...)`
- `ConsensusModeCPraos = 0`, `ConsensusModeTPraos = 1`

`f` (active slot coefficient) **must be an exact `*big.Rat`** (default `1/20`),
never a float. This matters for exact reproduction.

## Source files (the build is just these four + module files)

| File | Responsibility |
|------|----------------|
| `main.go` | CLI flags, orchestration. `checkSlotsInEpoch` builds one `LeaderEligibility` and, per slot, does `vrfInputForMode → signer.Prove → CheckVRFOutput` with a bounded worker pool. Emits a JSON `leaderSummary` with `assignedSlots`, `Ideal`, `Performance`. |
| `cardano.go` | Koios (v4) data access + nonce/VRF-hex helpers. **See the koios gotcha below.** |
| `timing.go` | Pure slot↔epoch↔time math. Constants: `systemStartUnixEpoch=1506203091`, `EpochLength=432000`, `FirstShelleyEpoch=208`, `SlotInEpochForNextNonce=302400`. |
| `vrf.go` | `vrfSeedFromCborHex` — extracts the 32-byte VRF seed from the `-vrf-skey-cborhex` input (bare 32-byte, `0x5820` bytestring, or `0x839820` 3-tuple). Pure-Go VRF (no cgo). |
| `main_test.go` | Unit tests: `parseRat`, `parseMode`, `vrfSeedFromCborHex`, the mode-aware VRF input, and a parity test asserting the `Prove`+`CheckVRFOutput` pipeline agrees with `IsSlotLeaderFromComponentsWithMode`. |

`go build ./...` ignores `unused/` (its nested `test_rs` has its own `go.mod`).

## Build / test / run

Toolchain: **Go ≥ 1.26.11** is required (gouroboros v0.220.4). The repo sets
`GOTOOLCHAIN=auto` so the correct toolchain is fetched automatically. A system
`go1.26.11` is already present.

```bash
make            # build ./llt        (equivalent: go build -o llt .)
make test       # go test -count=1 ./...
make ci         # fmt-check + vet + test + build  (use this to prove "all ok")
make run ARGS="--help"
make clean
```

`run_tests.sh` is a CI-style wrapper (same checks + an opt-in live Koios smoke
test). Env knobs: `SMOKE_EPOCH`, `SMOKE_POOL`, `SMOKE_VRF_SEED`, `KOIOS_API_URL`.

CLI flags (see `llt -h`): `-vrf-skey-cborhex`, `-pool-id-bech32`, `-f`
(rational, default `1/20`), `-consensus` (`cpraos|tpraos|praos`, default
`cpraos`), `-epoch-no`, `-all|-prev|-current|-next`, `-only-nonce`,
`-parallel-factor`, `-tz`.

Quirky flag interaction (intentional, pre-existing): if `-prev` or `-next` is
set without `-all`, `-current` is disabled.

## Dependencies (and why each exists)

- `github.com/blinklabs-io/gouroboros v0.220.4` — **the correctness core**:
  `consensus` (E34 + leader eligibility + pure-Go VRF signer) and `vrf`
  (`MkInputVrf`/`MkSeedTPraos`/`SeedL`). Pin to this version.
- `github.com/cardano-community/koios-go-client/v4` — fetch nonce, active stake,
  pool stake from the Koios REST API.
- `github.com/shopspring/decimal` — parse Koios stake fields into `uint64`.
- `github.com/spf13/pflag` — flag parsing.

## ⚠️ Known gotchas (read before touching data access)

1. **Koios v4 force-injects `order=epoch_no.desc`**, which the Koios v1 REST API
   rejects when combined with an epoch filter
   (`"column record.epoch_no does not exist"` → HTTP 400). This breaks
   `GetEpochInfo`/`GetEpochParams` and cannot be suppressed through the public
   client API. **Workaround already in `cardano.go`:** `getEpochInfoForNonce`
   fetches `epoch_info`/`epoch_params` via raw HTTP (`koiosGetJSON`) that omits
   `order`, reusing `kc.BaseURL()`. The pool-stake calls
   (`GetPoolStakeSnapshot`, `GetPoolHistory` in `pool.go`) do **not** inject
   `order`, so they keep using the typed client. Don't "fix" this by routing the
   epoch reads back through `GetEpochInfo`/`GetEpochParams` — it will 400 again.

2. **Host selection / network.** `koios.New()` defaults to `api.koios.rest`,
   which currently serves **testnet-style** data in this environment
   (epoch ~560, ~22 M ADA active stake). Mainnet is `api.koios.io/api/v1`
   (not reachable from this sandbox). Override with `KOIOS_API_URL=https://api.koios.io/api/v1`.
   For mainnet, confirm epoch numbers before trusting output.

3. **`--epoch-no` / `--only-nonce` need network.** With no reachable Koios host
   they fail with HTTP errors (not a bug in the consensus path).

4. **`f` is exact.** Keep `parseRat` returning `*big.Rat`; never substitute a
   float — that reintroduces the rounding divergence this project fixed.

5. **VRF key parsing** handles three CBOR shapes (see `vrf.go`); if a new shape
   appears, add a case there, not in the consensus call.

## History (the design rationale — don't regress this)

- **Original problem:** a from-scratch Go port used `shopspring/decimal` with
  `RoundDown`, while the Rust `cncli` reference used `pallas-math::FixedDecimal`
  (floor toward −∞, `10^34` scale) and `cardano-node` used Haskell `Data.Fixed`
  `E34`. Three different rounding modes → divergent `c`/`x` → wrong leader slots.
- **Fix:** delete the home-grown E34 (`math.go`) and the cgo libsodium VRF
  (`libsodium.go`); delegate to `gouroboros/consensus` (finite E34 + pure-Go
  VRF). Dingo (`BlinkLabs/dingo/ledger/leader/schedule.go`) is the in-production
  precedent: it calls `consensus.NewLeaderEligibility(...).CheckVRFOutput(...)`.
- **Now deleted (do not resurrect):** `math.go`, `libsodium.go`,
  `build_libsodium_helper.go`, the `echovl/cardano-go` fork dependency, and the
  node-CLI `getEta0` path (replaced by `eta0FromHTTP`, `f2lb.bardels.me`).
- **Koios upgraded v3 → v4** (this added the `order` workaround above).

## `unused/` (gitignored; ~1.5 GB, not part of the build)

Old trace logs (`*.trace.log`), the old cgo `go-leaderlog-tool` binary, the old
`test_rs/` experiments (a Rust `bigdecimal` harness `ll_test_rs` and an old Go
`shopspring/decimal` harness `ll_test_go`, plus a `pallas-math` stub
`ll_test_rs_new`), and stray `*.json` artifacts. Kept for reference; safe to
delete if you want a leaner clone.

## Reference repos (clone/checkout locations, if available)

- `gouroboros` (E34 + VRF, the authority we follow):
  `~/.local/gocode/pkg/mod/github.com/blinklabs-io/gouroboros@v0.220.4/`
  — key files: `consensus/e34.go`, `consensus/eligibility.go`,
  `consensus/leader.go`, `vrf/vrf.go`.
- `cncli` (Rust reference, `leaderlog`): `/home/marco/src/Others/cncli`
  (`src/nodeclient/leaderlog/{election,rational}.rs`).
- `pallas` (Rust `pallas-math`, `FixedDecimal`): `/home/marco/src/Others/pallas`
  (`pallas-math/src/math.rs`, `math_dashu.rs`; `DEFAULT_PRECISION=34`,
  `EPS=10^10`, `MAX_N=1000`, `BOUND_X=3`).
- `dingo` (in-production Go precedent for the eligibility call):
  `/home/marco/src/Others/BlinkLabs/dingo` (`ledger/leader/schedule.go`,
  `ScheduleFormatVersion=7`).
- `cardano-node` (Haskell consensus authority): `/home/marco/src/Others/cardano-node`.

## Suggested next steps / open items

- **Golden-vector parity test:** add a test comparing `llt` output for a fixed
  `(pool, nonce, f, epoch)` against a recorded `cncli` result (currently we only
  assert internal consistency with `IsSlotLeaderFromComponentsWithMode`).
- **Mainnet smoke:** run `make ci` + `SMOKE_EPOCH=... KOIOS_API_URL=https://api.koios.io/api/v1 ./run_tests.sh`
  from a host that can reach mainnet.
- Decide whether the koios host should be a CLI flag (`-koios-url`) instead of /
  in addition to the `KOIOS_API_URL` env var.
- `eta0FromHTTP` currently `panic`s on transport error; convert to a returned
  error for cleaner CLI UX.
