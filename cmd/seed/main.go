// Command seed creates the funding System account (if missing) and N empty
// Wallets, then prints their IDs as JSON for the harness. Wallets start at
// zero: money enters only through Top-ups, so every balance is backed by Entries.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"

	"github.com/google/uuid"

	"payment-ledger/internal/store"
)

type output struct {
	FundingID uuid.UUID   `json:"funding_id"`
	WalletIDs []uuid.UUID `json:"wallet_ids"`
}

func main() {
	wallets := flag.Int("wallets", 100, "number of Wallets to create")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	defer db.Close()

	var out output
	if out.FundingID, err = store.EnsureFundingAccount(ctx, db); err != nil {
		log.Fatalf("seed: %v", err)
	}
	if out.WalletIDs, err = store.CreateWallets(ctx, db, *wallets); err != nil {
		log.Fatalf("seed: %v", err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		log.Fatalf("seed: %v", err)
	}
}
