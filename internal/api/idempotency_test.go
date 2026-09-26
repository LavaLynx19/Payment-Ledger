package api

import (
	"bytes"
	"testing"

	ledgerv1 "payment-ledger/gen/ledger/v1"
	"payment-ledger/gen/ledger/v1/ledgerv1connect"
)

func TestRequestHash(t *testing.T) {
	topUp := &ledgerv1.TopUpRequest{WalletId: "w", Amount: 100}
	withdraw := &ledgerv1.WithdrawRequest{WalletId: "w", Amount: 100}

	a, _ := requestHash(ledgerv1connect.LedgerServiceTopUpProcedure, topUp)
	again, _ := requestHash(ledgerv1connect.LedgerServiceTopUpProcedure, &ledgerv1.TopUpRequest{WalletId: "w", Amount: 100})
	if !bytes.Equal(a, again) {
		t.Error("identical requests hashed differently")
	}

	b, _ := requestHash(ledgerv1connect.LedgerServiceWithdrawProcedure, withdraw)
	if bytes.Equal(a, b) {
		t.Error("TopUp and Withdraw with identical bodies share a hash")
	}

	c, _ := requestHash(ledgerv1connect.LedgerServiceTopUpProcedure, &ledgerv1.TopUpRequest{WalletId: "w", Amount: 101})
	if bytes.Equal(a, c) {
		t.Error("different amounts share a hash")
	}
}
