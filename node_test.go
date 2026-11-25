package main

import (
	"context"
	"os"
	"testing"
	"time"

	ouroboros "github.com/blinklabs-io/gouroboros"
)

// TestNodeProbe is an opt-in live probe of a cardano-node N2C socket via
// gouroboros local-state-query. It is skipped unless NODE_PROBE=1, so the
// default `go test ./...` stays hermetic (no network, no node).
//
// Env:
//
//	NODE_PROBE=1                              enable
//	NODE_SOCKET_PATH=…/bart.bardels.me_33333.sock   socket (required)
//	NODE_NETWORK=mainnet|preprod|testnet      (default mainnet)
func TestNodeProbe(t *testing.T) {
	if os.Getenv("NODE_PROBE") == "" {
		t.Skip("set NODE_PROBE=1 to run the live node probe")
	}
	socket := os.Getenv("NODE_SOCKET_PATH")
	if socket == "" {
		t.Skip("NODE_SOCKET_PATH not set")
	}
	network := ouroboros.NetworkCardanoMainnet
	switch os.Getenv("NODE_NETWORK") {
	case "preprod", "preproduction", "testnet":
		network = ouroboros.NetworkCardanoPreprod
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := probeNodeSnapshot(ctx, socket, network, 5); err != nil {
		t.Fatalf("node probe: %v", err)
	}
}
