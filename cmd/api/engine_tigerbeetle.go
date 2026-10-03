//go:build tigerbeetle

package main

import (
	"time"

	"payment-ledger/internal/api"
	"payment-ledger/internal/tbengine"
)

// tigerbeetleEngine serves the API from TigerBeetle (A§9.7).
func tigerbeetleEngine(address string, holdTTL time.Duration) (api.Engine, func(), error) {
	e, err := tbengine.New(address, holdTTL)
	if err != nil {
		return nil, nil, err
	}
	return e, e.Close, nil
}
