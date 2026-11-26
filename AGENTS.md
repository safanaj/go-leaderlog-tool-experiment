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

## Source files (the build is these + module files)

| File | Responsibility |
|------|----------------|
| `datasource.go` | The `DataSource` interface (`GetEpochInfo`/`GetPoolStakeForEpoch`/`GetStakeSnapshots`/`GetCandidateNonce`/`Name`/`Close`) + the `EpochInfo`/`PoolStakeSnapshot` types. All data access goes through it. |
| `main.go` | CLI flags, orchestration. `run()` (called by `main`, which maps errors to exit status 1 + stderr) drives everything through a `DataSource` built by `newDataSource` (`-data-source koios\|db\|node`). `checkSlotsInEpoch` builds one `LeaderEligibility` and, per slot, does `vrfInputForMode → signer.Prove → CheckVRFOutput` with a bounded worker pool; any slot-level error fails the whole run (first error wins) so a transient failure can never silently drop an assigned slot. Emits a JSON `leaderSummary` with `assignedSlots`, `Ideal`, `Performance`. |
| `cardano.go` | `koiosDataSource` (default source): the Koios (v4) data access + nonce/VRF-hex helpers. **See the koios gotchas below.** |
| `db.go` | `dbDataSource`: reads the inputs directly from a cexplorer (db-sync) Postgres DB over client-cert TLS (the verified SQL from PLAN.md §1). |
| `node.go` | `nodeSource`: a running cardano-node's N2C socket via gouroboros `localstatequery` (tip-derived: serves the current epoch + the three chain-dep-state nonces only). |
| `timing.go` | Pure slot↔epoch↔time math. Constants: `systemStartUnixEpoch=1506203091`, `EpochLength=432000`, `FirstShelleyEpoch=208`, `SlotInEpochForNextNonce=302400`. |
| `vrf.go` | `vrfSeedFromCborHex` — extracts the 32-byte VRF seed from the `-vrf-skey-cborhex` input (bare 32-byte, `0x5820` bytestring, `0x5840` 64-byte extended-key bytestring i.e. modern `VrfSigningKey_PraosVRF`, or `0x839820` 3-tuple). Pure-Go VRF (no cgo). |
| `main_test.go` | Unit tests: `parseRat`, `parseMode`, `vrfSeedFromCborHex`, the mode-aware VRF input, a parity test asserting the `Prove`+`CheckVRFOutput` pipeline agrees with `IsSlotLeaderFromComponentsWithMode`, and fail-fast tests for the slot sweep (injected `Prove` failure, pre-Shelley epoch rejection, clamped negative `-parallel-factor`). |
| `cardano_test.go` | Unit tests for `eta0FromHTTP` (httptest server: 200 trim, 404 → "not yet", 5xx/transport/timeout → error, never a panic). |
| `koios_test.go` | End-to-end tests of `koiosDataSource` against an httptest Koios fixture (raw epoch reads, typed pool reads, `KOIOS_API_URL` option mapping, `parseNetwork`). |
| `db_test.go` | `checkDBCerts` TLS-material validation + `newDataSource` selection errors (no live DB needed). |

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

`run_tests.sh` is a CI-style wrapper (same checks + opt-in live smoke tests:
Koios, db parity, and node). Env knobs: `SMOKE_EPOCH`, `SMOKE_POOL`,
`SMOKE_VRF_SEED`, `KOIOS_API_URL`, `SMOKE_DB=1` + `SMOKE_DB_{CERT,KEY,CA}`
(+ optional `SMOKE_DB_DSN`, `SMOKE_DB_EPOCH`), `SMOKE_NODE_SOCKET`.

CLI flags (see `llt -h`): `-vrf-skey-cborhex`, `-pool-id-bech32`, `-f`
(rational, default `1/20`), `-consensus` (`cpraos|tpraos|praos`, default
`cpraos`), `-epoch-no`, `-all|-prev|-current|-next`, `-only-nonce`,
`-parallel-factor`, `-tz`, and the data-source flags below.

Quirky flag interaction (intentional, pre-existing): if `-prev` or `-next` is
set without `-all`, `-current` is disabled.

## Data sources (`-data-source`)

All external inputs flow through the `DataSource` interface (`datasource.go`).
The consensus core never changes — only the inputs vary.

| Source | Flag | What it is / limitations |
|--------|------|---------------------------|
| `koios` (default) | — | Koios REST API (v4 client). The candidate nonce for `-next` comes from the `f2lb.bardels.me` HTTP endpoint (Koios does not expose it). |
| `db` | `-db-dsn` (default: the `marge.bardels.me` cexplorer DSN), `-db-cert-file`/`-db-key-file`/`-db-ca-cert-file` (all three **required**; env `DB_DSN`, `DB_CERT_FILE`, `DB_KEY_FILE`, `DB_CA_CERT_FILE`) | cexplorer (cardano-db-sync) Postgres, client-certificate TLS. Uses the SQL verified in PLAN.md §1/§11. **Its candidate nonce only exists once the upcoming epoch actually starts** (epoch_param's E+1 row) — `-next` before the boundary errors "not yet available"; use `koios`/`node` for an early candidate nonce. |
| `node` | `-node-socket` (**required**; env `NODE_SOCKET`), `-node-network` (`mainnet`\|`preprod`\|`preview`, env `NODE_NETWORK`) | A running cardano-node's N2C Unix socket via gouroboros `localstatequery` (no TLS — N2C is local-only). **Tip-derived**: serves the current epoch end-to-end (Set nonce + tip pool distribution) and the chain-dep-state nonces (Go/Set/Mark for `-only-nonce`), but **not** `-epoch-no`, and `-prev`/`-next` full runs fail loudly on the missing Go/Mark stake snapshots. The tip snapshot is fetched once per process (all methods read one consistent view). |

Parity (verified 2026-11, `run_tests.sh` + PLAN.md §5 step 7): koios == db for
a past epoch (full JSON diff), and node == db == koios for the current epoch.

## Dependencies (and why each exists)

- `github.com/blinklabs-io/gouroboros v0.220.4` — **the correctness core**:
  `consensus` (E34 + leader eligibility + pure-Go VRF signer), `vrf`
  (`MkInputVrf`/`MkSeedTPraos`/`SeedL`), and `localstatequery` + `ledger/common`
  (`NewPoolIdFromBech32`) for the node source. Pin to this version.
- `github.com/cardano-community/koios-go-client/v4` — fetch nonce, active stake,
  pool stake from the Koios REST API.
- `github.com/jackc/pgx/v5` (pgxpool + stdlib) — the cexplorer Postgres source.
  Note: closing the `sql.DB` from `stdlib.OpenDBFromPool` does **not** close the
  pool — `dbDataSource.Close` closes both.
- `github.com/shopspring/decimal` — parse Koios stake fields into `uint64`.
- `github.com/spf13/pflag` — flag parsing.

## ⚠️ Known gotchas (read before touching data access)

1. **Koios v4 force-injects `order=epoch_no.desc`**, which the Koios v1 REST API
   rejects when combined with an epoch filter
   (`"column record.epoch_no does not exist"` → HTTP 400). This breaks
   `GetEpochInfo`/`GetEpochParams` and cannot be suppressed through the public
   client API. **Workaround in `cardano.go`:** `koiosDataSource.GetEpochInfo`
   fetches `epoch_info`/`epoch_params` via raw HTTP (`koiosGetJSON`) that omits
   `order`, reusing the client's own base URL. The pool-stake calls
   (`GetPoolStakeSnapshot`, `GetPoolHistory`) do **not** inject `order`, so they
   keep using the typed client. Don't "fix" this by routing the epoch reads back
   through `GetEpochInfo`/`GetEpochParams` — it will 400 again.

2. **Host selection / network.** `koios.New()` defaults to `api.koios.rest`
   (which currently serves mainnet data from this sandbox). Override with
   `KOIOS_API_URL=https://api.koios.io/api/v1` — it is parsed into client
   options in `NewKoiosDataSource`, so it applies to **both** the raw HTTP
   reads and the typed client (a single host per run; don't reintroduce a
   split where some reads ignore the override). The URL must have the shape
   `scheme://host[:port]/api/<version>`. Confirm epoch numbers before trusting
   output.

3. **`--epoch-no` / `--only-nonce` need network** (koios/db) or a live node
   socket (node). With no reachable host they fail with transport errors (not
   a bug in the consensus path).

4. **`f` is exact.** Keep `parseRat` returning `*big.Rat`; never substitute a
   float — that reintroduces the rounding divergence this project fixed.

5. **VRF key parsing** handles four CBOR shapes (see `vrf.go`); if a new shape
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
- **Data sources abstracted** (PLAN.md): all external reads now go through the
  `DataSource` interface — koios (default, unchanged output), a cexplorer
  Postgres source (`db`, pgx/v5 + client-cert TLS, the verified SQL), and a
  node N2C source (`node`, gouroboros `localstatequery`, tip-derived). The
  E34/VRF/`f`-rational code did not change; koios↔db↔node parity was verified
  by full-JSON diffs (see "Data sources" above).

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
  assert internal consistency with `IsSlotLeaderFromComponentsWithMode` and
  cross-source parity).
- Decide whether the koios host should be a CLI flag (`-koios-url`) instead of /
  in addition to the `KOIOS_API_URL` env var.
- The db source's `-next` needs the epoch boundary (no early candidate nonce);
  consider computing it from the DB's evolving-nonce data or falling back to the
  node/koios source when `-data-source db -next` is asked for early.
