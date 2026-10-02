//go:build !tigerbeetle

package main

import (
	"context"
	"errors"

	"payment-ledger/internal/checker"
)

func tigerbeetlePass(context.Context, []checker.Ack) ([]checker.Result, error) {
	return nil, errors.New("LEDGER_ENGINE=tigerbeetle needs a binary built with -tags tigerbeetle (image payment-ledger:tb)")
}
