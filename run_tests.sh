#!/usr/bin/env bash
#
# run_tests.sh — verify go-leaderlog-tool is healthy and build the llt binary.
#
# What it does, in order:
#   1. gofmt   : source must be gofmt-clean
#   2. vet     : go vet must pass
#   3. test    : go test ./... must pass (the E34/consensus parity tests)
#   4. build   : produces the ./llt executable
#   5. smoke   : (opt-in) live Koios checks; skipped unless SMOKE_EPOCH is set
#
# Optional env vars:
#   KOIOS_API_URL   Koios REST base, e.g. https://api.koios.io/api/v1
#   SMOKE_EPOCH     run `llt --only-nonce` for this epoch (needs network)
#   SMOKE_POOL      bech32 pool id; if set with SMOKE_VRF_SEED, run a full check
#   SMOKE_VRF_SEED  64-hex (32-byte) VRF skey cborhex for the full smoke check
#
# Exit status is non-zero if any mandatory step fails.

set -euo pipefail

cd "$(dirname "$0")"
export GOTOOLCHAIN=auto

pass() { printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; }
step() { printf '\033[1m== %s ==\033[0m\n' "$1"; }

rc=0
check() { # check <label> <cmd...>
  local label="$1"; shift
  step "$label"
  if "$@"; then pass "$label"; else fail "$label"; rc=1; fi
}

# --- 1. gofmt --------------------------------------------------------------
step "gofmt"
if out=$(gofmt -l . 2>&1 | grep -v '^unused/'); [ -n "$out" ]; then
  fail "gofmt: unformatted files"
  echo "$out"
else
  pass "gofmt: all files formatted"
fi

# --- 2. vet ----------------------------------------------------------------
check "go vet" go vet ./...

# --- 3. tests --------------------------------------------------------------
check "go test" go test -count=1 ./...

# --- 4. build the llt executable ------------------------------------------
step "build ./llt"
if go build -o llt .; then
  pass "build ./llt ($(du -h llt | cut -f1))"
else
  fail "build ./llt"; rc=1
fi

# --- 5. optional live smoke tests -----------------------------------------
if [ -n "${SMOKE_EPOCH:-}" ]; then
  step "smoke: nonce for epoch ${SMOKE_EPOCH}"
  nonce=$(./llt --epoch-no "${SMOKE_EPOCH}" --only-nonce 2>/dev/null || true)
  if printf '%s' "${nonce}" | grep -Eq '^[0-9a-f]{64}$'; then
    pass "smoke: nonce = ${nonce}"
  else
    fail "smoke: expected 64-hex nonce, got: ${nonce:-<empty>}"
    rc=1
  fi

  if [ -n "${SMOKE_POOL:-}" ] && [ -n "${SMOKE_VRF_SEED:-}" ]; then
    step "smoke: full slot check epoch ${SMOKE_EPOCH} pool ${SMOKE_POOL}"
    if out=$(./llt --epoch-no "${SMOKE_EPOCH}" \
        --vrf-skey-cborhex "${SMOKE_VRF_SEED}" \
        --pool-id-bech32 "${SMOKE_POOL}" 2>&1); then
      # a JSON summary must contain an assignedSlots array
      if printf '%s' "${out}" | grep -q '"assignedSlots"'; then
        slots=$(printf '%s' "${out}" | grep -c '"epoch"' || true)
        pass "smoke: full check ran (assigned slots: ${slots})"
      else
        fail "smoke: no JSON summary in output"; rc=1
      fi
    else
      fail "smoke: full check errored: ${out}"; rc=1
    fi
  fi
else
  step "smoke: SKIPPED (set SMOKE_EPOCH to run; add SMOKE_POOL+SMOKE_VRF_SEED for a full check)"
fi

# --- summary ---------------------------------------------------------------
printf '\n'
if [ "${rc}" -eq 0 ]; then
  printf '\033[32mALL CHECKS PASSED\033[0m — llt built at ./llt\n'
else
  printf '\033[31mSOME CHECKS FAILED\033[0m\n'
fi
exit "${rc}"
