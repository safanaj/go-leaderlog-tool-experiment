package main

/*
#cgo CFLAGS: -Wall -I${SRCDIR}/_c_libsodium_built/include
#cgo LDFLAGS: -L${SRCDIR}/_c_libsodium_built -l:libsodium.a
#include <sodium/core.h>
#include <sodium/crypto_vrf.h>
#include <sodium/crypto_vrf_ietfdraft03.h>
#include <sodium.h>
#include <string.h>
#include <stdio.h>
#include <stddef.h>
*/
import "C"
import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math/big"

	"golang.org/x/crypto/blake2b"
)

func getVrfSKeyDataFromCborHex(cborHex string) []byte {
	// short way
	return []byte(cborHex[4:])

	// // long way
	// var vrfSkeyData [64]byte
	// cborData, err := hex.DecodeString(cborHex)
	// if err != nil {
	// 	panic(err)
	// }
	// err = cbor.Unmarshal(cborData, &vrfSkeyData)
	// if err != nil {
	// 	panic(err)
	// }
	// return vrfSkeyData[:]
}

func mkSeed(slot int, eta0 string) []byte {
	eta0bytes, _ := hex.DecodeString(eta0)
	buff := new(bytes.Buffer)
	binary.Write(buff, binary.BigEndian, int64(slot))
	h := blake2b.Sum256(append(buff.Bytes(), eta0bytes...))
	return h[:]
}

func initialize_libsodium() int {
	return int(C.sodium_init())
}

func vrfCert(seed, vrfSkey []byte) *big.Int {
	// proofbytes_ := C.crypto_vrf_ietfdraft03_proofbytes()
	// proofBytes := C.GoBytes(unsafe.Pointer(&proofbytes_), C.int(C.crypto_vrf_PROOFBYTES))
	proofBytes := make([]byte, C.int(C.crypto_vrf_ietfdraft03_proofbytes()))
	C.crypto_vrf_prove(
		(*C.uchar)((&proofBytes[0])),
		(*C.uchar)(&vrfSkey[0]),
		(*C.uchar)((&seed[0])),
		(C.ulonglong)(len(seed)))
	// outbytes_ := C.crypto_vrf_outputbytes()
	// outBytes := C.GoBytes(unsafe.Pointer(&outbytes_), C.int(C.crypto_vrf_OUTPUTBYTES))
	outBytes := make([]byte, C.int(C.crypto_vrf_outputbytes()))
	C.crypto_vrf_proof_to_hash((*C.uchar)((&outBytes[0])), (*C.uchar)((&proofBytes[0])))
	return big.NewInt(0).SetBytes(outBytes)
}

// should double check with: https://github.com/cardano-community/cncli/blob/develop/src/nodeclient/leaderlog.rs#L327
func vrfLeaderValue(raw *big.Int) *big.Int {
	var val [64]byte
	raw.FillBytes(val[:])
	h := blake2b.Sum256(append([]byte{0x4C}, val[:]...))
	return big.NewInt(0).SetBytes(h[:])
}

func getVrfMaxValue() *big.Int { return big.NewInt(0).Exp(big.NewInt(2), big.NewInt(256), nil) }

func getVrfLeaderValue(slot int, eta0, cborHex string) *big.Int {
	seed := mkSeed(slot, eta0)
	cert := vrfCert(seed, getVrfSKeyDataFromCborHex(cborHex))
	return vrfLeaderValue(cert)
}
