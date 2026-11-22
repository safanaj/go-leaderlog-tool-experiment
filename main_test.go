package main

import (
	"math/big"
	"testing"

	consensus "github.com/blinklabs-io/gouroboros/consensus"
	"github.com/blinklabs-io/gouroboros/vrf"
)

func TestParseRat(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"1/20", "1/20"},
		{"1/4", "1/4"},
		{"0.05", "1/20"}, // 0.05 == 1/20
		{"1/10000000000000000000", "1/10000000000000000000"},
	} {
		r, err := parseRat(tc.in)
		if err != nil {
			t.Fatalf("parseRat(%q): %v", tc.in, err)
		}
		if r.RatString() != tc.want {
			t.Errorf("parseRat(%q) = %s, want %s", tc.in, r.RatString(), tc.want)
		}
	}
	if _, err := parseRat("abc"); err == nil {
		t.Error("parseRat(abc) should fail")
	}
	if _, err := parseRat("1/0"); err == nil {
		t.Error("parseRat(1/0) should fail")
	}
}

func TestParseMode(t *testing.T) {
	for _, m := range []string{"cpraos", "praos", "cpbraos", ""} {
		if mode, err := parseMode(m); err != nil || mode != consensus.ConsensusModeCPraos {
			t.Errorf("parseMode(%q) = %v, %v; want CPraos", m, mode, err)
		}
	}
	if mode, err := parseMode("tpraos"); err != nil || mode != consensus.ConsensusModeTPraos {
		t.Errorf("parseMode(tpraos) = %v, %v; want TPraos", mode, err)
	}
	if _, err := parseMode("bogus"); err == nil {
		t.Error("parseMode(bogus) should fail")
	}
}

func TestVrfSeedFromCborHex(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	// bare 32-byte seed
	if got, err := vrfSeedFromCborHex(encodeHex(seed)); err != nil {
		t.Fatal(err)
	} else if len(got) != 32 {
		t.Fatalf("bare: got %d bytes", len(got))
	}
	// CBOR byte string: 0x58 0x20 <32 bytes>
	byt := append([]byte{0x58, 0x20}, seed...)
	if got, err := vrfSeedFromCborHex(encodeHex(byt)); err != nil {
		t.Fatal(err)
	} else if string(got) != string(seed) {
		t.Errorf("bytstring seed mismatch")
	}
	// CBOR 3-tuple: 0x83 0x98 0x20 <32-byte seed> ...
	tuple := append([]byte{0x83, 0x98, 0x20}, seed...)
	if got, err := vrfSeedFromCborHex(encodeHex(tuple)); err != nil {
		t.Fatal(err)
	} else if string(got) != string(seed) {
		t.Errorf("tuple seed mismatch")
	}
}

func TestVrfInputForMode(t *testing.T) {
	nonce := make([]byte, 32)
	slot := int64(12345678)
	if got, err := vrfInputForMode(int(slot), nonce, consensus.ConsensusModeCPraos); err != nil {
		t.Fatal(err)
	} else {
		want, _ := vrf.MkInputVrf(slot, nonce)
		if string(got) != string(want) {
			t.Error("CPraos vrf input mismatch")
		}
	}
	if got, err := vrfInputForMode(int(slot), nonce, consensus.ConsensusModeTPraos); err != nil {
		t.Fatal(err)
	} else {
		want, _ := vrf.MkSeedTPraos(slot, nonce, vrf.SeedL())
		if string(got) != string(want) {
			t.Error("TPraos vrf input mismatch")
		}
	}
}

// TestLeaderEligibilityPipeline exercises the exact path the tool uses
// (signer.Prove + CheckVRFOutput) and asserts it agrees with gouroboros's
// one-shot IsSlotLeaderFromComponentsWithMode, which is the E34 predicate
// cardano-node uses.
func TestLeaderEligibilityPipeline(t *testing.T) {
	const poolStake = 1_000_000_000
	const totalStake = 25_000_000_000_000_000

	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i*7 + 3)
	}
	signer, err := consensus.NewSimpleVRFSigner(seed)
	if err != nil {
		t.Fatal(err)
	}

	nonce := make([]byte, 32)
	for i := range nonce {
		nonce[i] = byte(i + 0x11)
	}
	f := big.NewRat(1, 20)

	// Full sweep over a small slot window.
	for slot := int64(1000); slot < 1064; slot++ {
		input, err := vrfInputForMode(int(slot), nonce, consensus.ConsensusModeCPraos)
		if err != nil {
			t.Fatal(err)
		}
		_, output, err := signer.Prove(input)
		if err != nil {
			t.Fatal(err)
		}
		elig, err := consensus.NewLeaderEligibility(poolStake, totalStake, f, consensus.ConsensusModeCPraos)
		if err != nil {
			t.Fatal(err)
		}
		got, err := elig.CheckVRFOutput(output)
		if err != nil {
			t.Fatal(err)
		}
		want, err := consensus.IsSlotLeaderFromComponentsWithMode(
			output, poolStake, totalStake, f, consensus.ConsensusModeCPraos,
		)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("slot %d: pipeline=%v, IsSlotLeader=%v", slot, got, want)
		}
	}
}

// TestFullStakeF1Unconditional: with f=1 and a positive pool stake, every slot
// must be eligible regardless of the VRF output.
func TestFullStakeF1Unconditional(t *testing.T) {
	seed := make([]byte, 32)
	signer, err := consensus.NewSimpleVRFSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	f := big.NewRat(1, 1)
	elig, err := consensus.NewLeaderEligibility(100, 100, f, consensus.ConsensusModeCPraos)
	if err != nil {
		t.Fatal(err)
	}
	for slot := int64(0); slot < 10; slot++ {
		input, _ := vrf.MkInputVrf(slot, nonce)
		_, output, err := signer.Prove(input)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := elig.CheckVRFOutput(output)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("f=1 full stake: slot %d not eligible, want true", slot)
		}
	}
}

func encodeHex(b []byte) string {
	const hextable = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hextable[v>>4]
		out[i*2+1] = hextable[v&0x0f]
	}
	return string(out)
}
