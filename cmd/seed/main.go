// Command seed creates each shard's funding System account (if missing) and
// N empty Wallets spread round-robin across shards, then prints their IDs as
// JSON for the harness. Wallets start at zero: money enters only through
// Top-ups, so every balance is backed by Entries.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"

	"github.com/google/uuid"

	"payment-ledger/internal/env"
	"payment-ledger/internal/store"
)

type output struct {
	FundingID  uuid.UUID   `json:"funding_id"`  // shard 0's, for single-shard scenarios
	FundingIDs []uuid.UUID `json:"funding_ids"` // one per shard, in shard order
	WalletIDs  []uuid.UUID `json:"wallet_ids"`
}

func main() {
	wallets := flag.Int("wallets", 100, "number of Wallets to create")
	flag.Parse()

	if env.Or("LEDGER_ENGINE", "postgres") == "tigerbeetle" {
		funding, ids, err := seedTigerBeetle(env.Must("TB_ADDRESS"), *wallets)
		if err != nil {
			log.Fatalf("seed: %v", err)
		}
		out := output{FundingID: funding, FundingIDs: []uuid.UUID{funding}, WalletIDs: ids}
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			log.Fatalf("seed: %v", err)
		}
		return
	}

	ctx := context.Background()
	shards, err := store.OpenShards(ctx, env.ShardURLs())
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	defer shards.Close()

	var out output
	for i := range shards.N() {
		id, err := store.EnsureFundingAccountOn(ctx, shards.Pool(i), i)
		if err != nil {
			log.Fatalf("seed: shard %d: %v", i, err)
		}
		out.FundingIDs = append(out.FundingIDs, id)
	}
	out.FundingID = out.FundingIDs[0]
	if out.WalletIDs, err = shards.CreateWallets(ctx, *wallets); err != nil {
		log.Fatalf("seed: %v", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		log.Fatalf("seed: %v", err)
	}
}
