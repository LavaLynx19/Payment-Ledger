//go:build tigerbeetle

package main

import (
	"github.com/google/uuid"

	"payment-ledger/internal/tbengine"
)

// seedTigerBeetle creates n Wallets in TigerBeetle and returns the funding id.
func seedTigerBeetle(address string, n int) (uuid.UUID, []uuid.UUID, error) {
	e, err := tbengine.New(address, 0)
	if err != nil {
		return uuid.Nil, nil, err
	}
	defer e.Close()
	ids, err := e.CreateWallets(n)
	return e.FundingID(), ids, err
}
