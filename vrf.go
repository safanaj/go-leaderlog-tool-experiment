package main

import (
	"encoding/hex"
	"fmt"
)

// vrfSeedFromCborHex extracts the 32-byte VRF secret seed from the CBOR
// hex-encoded VRF signing key that the CLI accepts via -vrf-skey-cborhex.
//
// Cardano VRF skeys are represented in a couple of CBOR shapes:
//
//   - a bare 32-byte seed (64 hex chars), or
//   - a CBOR byte string "0x5820 <32 bytes>", or
//   - a CBOR 3-tuple "0x83 0x98 0x20 <32-byte seed> ..." (type, seed, public key).
//
// In every case the VRF signing key is the 32-byte seed, which is exactly what
// gouroboros' consensus.NewSimpleVRFSigner consumes.
func vrfSeedFromCborHex(cborHex string) ([]byte, error) {
	raw, err := hex.DecodeString(cborHex)
	if err != nil {
		return nil, fmt.Errorf("invalid vrf skey cbor hex: %w", err)
	}

	switch {
	// Bare 32-byte seed.
	case len(raw) == 32:
		return raw, nil

	// CBOR byte string: 0x58 0x20 = "32-byte bytestring".
	case len(raw) >= 34 && raw[0] == 0x58 && raw[1] == 0x20:
		return raw[2:34], nil

	// CBOR 3-element array: 0x83 0x98 0x20 = array [uint 0, <32-byte seed>, ...].
	case len(raw) >= 35 && raw[0] == 0x83 && raw[1] == 0x98 && raw[2] == 0x20:
		return raw[3:35], nil
	}

	return nil, fmt.Errorf(
		"unrecognized vrf skey cbor shape (len %d, want bare 32-byte seed, 0x5820 bytestring, or 0x839820 tuple)",
		len(raw))
}
