//go:build !tigerbeetle

package main

import (
	"errors"

	"github.com/google/uuid"
)

func seedTigerBeetle(string, int) (uuid.UUID, []uuid.UUID, error) {
	return uuid.Nil, nil, errors.New("LEDGER_ENGINE=tigerbeetle needs a binary built with -tags tigerbeetle (image payment-ledger:tb)")
}
