package main

import (
	"encoding/hex"
	"fmt"
)

// vrfSeedFromCborHex extracts the 32-byte VRF secret seed from the CBOR
// hex-encoded VRF signing key that the CLI accepts via -vrf-skey-cborhex.
//
// Cardano VRF skeys are represented in a few CBOR shapes:
//
//   - a bare 32-byte seed (64 hex chars), or
//   - a CBOR byte string "0x5820 <32 bytes>", or
//   - a CBOR 3-tuple "0x83 0x98 0x20 <32-byte seed> ..." (type, seed, public key), or
//   - a CBOR 64-byte byte string "0x5840 <32-byte seed> <32-byte public key>"
//     (the modern cardano-cli "VrfSigningKey_PraosVRF" shape; cncli takes the
//     first 32 bytes of the same payload).
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

	// CBOR byte string: 0x58 0x40 = "64-byte bytestring" (seed || public key).
	// The extended-key form written by modern cardano-cli as
	// VrfSigningKey_PraosVRF; the VRF secret is the leading 32-byte seed.
	case len(raw) >= 66 && raw[0] == 0x58 && raw[1] == 0x40:
		return raw[2:34], nil

	// CBOR 3-element array: 0x83 0x98 0x20 = array [uint 0, <32-byte seed>, ...].
	case len(raw) >= 35 && raw[0] == 0x83 && raw[1] == 0x98 && raw[2] == 0x20:
		return raw[3:35], nil
	}

	return nil, fmt.Errorf(
		"unrecognized vrf skey cbor shape (len %d, want bare 32-byte seed, 0x5820 bytestring, 0x5840 extended-key bytestring, or 0x839820 tuple)",
		len(raw))
}
