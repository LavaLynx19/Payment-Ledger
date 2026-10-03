//go:build tigerbeetle

package main

import (
	"context"

	"payment-ledger/internal/checker"
	"payment-ledger/internal/env"
	"payment-ledger/internal/tbengine"
)

func tigerbeetlePass(_ context.Context, acks []checker.Ack) ([]checker.Result, error) {
	e, err := tbengine.New(env.Must("TB_ADDRESS"), 0)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	return e.Audit(acks)
}
