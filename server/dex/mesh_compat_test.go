// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package dex

import (
	"testing"
	"time"

	dexpkg "decred.org/dcrdex/dex"
	"decred.org/dcrdex/server/meshevents"
)

func TestBuildMeshCompatSnapshotIgnoresLocalOnlyFields(t *testing.T) {
	cfgA := &DexConf{
		DataDir:          "/tmp/a",
		Network:          dexpkg.Testnet,
		BroadcastTimeout: 12 * time.Minute,
		TxWaitExpiration: 2 * time.Minute,
		CancelThreshold:  0.95,
		FreeCancels:      true,
		PenaltyThreshold: 20,
		NodeRelayAddr:    "127.0.0.1:1000",
		Assets: []*Asset{
			{
				Symbol:     "dcr",
				Network:    "testnet",
				MaxFeeRate: 10,
				SwapConf:   2,
				BondAmt:    100,
				BondConfs:  3,
			},
		},
		Markets: []*dexpkg.MarketInfo{
			{
				Name:                   "dcr_btc",
				Base:                   42,
				Quote:                  0,
				LotSize:                1e8,
				ParcelSize:             2,
				RateStep:               1e3,
				EpochDuration:          20000,
				MarketBuyBuffer:        1.25,
				MaxUserCancelsPerEpoch: 5,
			},
		},
	}
	cfgB := *cfgA
	cfgB.DataDir = "/tmp/b"
	cfgB.NodeRelayAddr = "127.0.0.1:2000"
	cfgB.MeshCfg = &MeshConfig{PeerAddr: "127.0.0.1:3000"}

	snapA, err := buildMeshCompatSnapshot(cfgA)
	if err != nil {
		t.Fatalf("buildMeshCompatSnapshot(cfgA) error: %v", err)
	}
	snapB, err := buildMeshCompatSnapshot(&cfgB)
	if err != nil {
		t.Fatalf("buildMeshCompatSnapshot(cfgB) error: %v", err)
	}

	if snapA.Hash != snapB.Hash {
		t.Fatalf("hash mismatch for local-only config changes: %x != %x", snapA.Hash, snapB.Hash)
	}
	if snapA.Config.EventSchemaVersion != meshevents.EventSchemaVersion {
		t.Fatalf("event schema version = %d, want %d",
			snapA.Config.EventSchemaVersion, meshevents.EventSchemaVersion)
	}
}

func TestBuildMeshCompatSnapshotDetectsRelevantChange(t *testing.T) {
	cfg := &DexConf{
		Network:          dexpkg.Testnet,
		BroadcastTimeout: 12 * time.Minute,
		TxWaitExpiration: 2 * time.Minute,
		CancelThreshold:  0.95,
		FreeCancels:      true,
		PenaltyThreshold: 20,
		Assets: []*Asset{
			{
				Symbol:     "dcr",
				Network:    "testnet",
				MaxFeeRate: 10,
				SwapConf:   2,
				RegXPub:    "xpub-1",
			},
		},
		Markets: []*dexpkg.MarketInfo{
			{
				Name:                   "dcr_btc",
				Base:                   42,
				Quote:                  0,
				LotSize:                1e8,
				ParcelSize:             2,
				RateStep:               1e3,
				EpochDuration:          20000,
				MarketBuyBuffer:        1.25,
				MaxUserCancelsPerEpoch: 5,
			},
		},
	}

	snapA, err := buildMeshCompatSnapshot(cfg)
	if err != nil {
		t.Fatalf("buildMeshCompatSnapshot(cfg) error: %v", err)
	}

	cfg.Assets[0].RegXPub = "xpub-2"
	snapB, err := buildMeshCompatSnapshot(cfg)
	if err != nil {
		t.Fatalf("buildMeshCompatSnapshot(modified cfg) error: %v", err)
	}

	if snapA.Hash == snapB.Hash {
		t.Fatalf("hash did not change after relevant config change")
	}
}
