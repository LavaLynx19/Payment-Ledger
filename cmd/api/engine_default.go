//go:build !tigerbeetle

package main

import (
	"errors"
	"time"

	"payment-ledger/internal/api"
)

// tigerbeetleEngine is unavailable: the TigerBeetle client links on Linux only,
// so the engine needs `-tags tigerbeetle` (Dockerfile target `tigerbeetle`).
func tigerbeetleEngine(string, time.Duration) (api.Engine, func(), error) {
	return nil, nil, errors.New("LEDGER_ENGINE=tigerbeetle needs a binary built with -tags tigerbeetle (image payment-ledger:tb)")
}
