package ledger

import (
	"context"
	"errors"
	"testing"
)

func TestReceivableBlocksDebitsUntilRepaid(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	_, debtor, id := paid(t, l, 100)
	spend(t, l, debtor, 80)
	reverse(t, l, id) // recovers 20, debtor owes 80
	drain(t, l)
	other := wallets(t, l, 1)[0]

	// Every debit except a Repayment is blocked, even with funds available.
	fund(t, l, debtor, 50)
	var owes *ReceivableOpenError
	blocked := map[string]func() error{
		"transfer": func() error {
			_, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: debtor, DestID: other, Amount: 1})
			return err
		},
		"hold": func() error {
			_, _, err := l.PlaceHold(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: debtor, DestID: other, Amount: 1})
			return err
		},
		"withdraw": func() error {
			_, err := l.Withdraw(ctx, key(), []byte("h"), debtor, 1)
			return err
		},
	}
	for name, debit := range blocked {
		if err := debit(); !errors.As(err, &owes) || owes.Owed != 80 {
			t.Errorf("%s err = %v, want receivable open (owes 80)", name, err)
		}
	}

	if _, err := l.Repay(ctx, key(), []byte("h"), debtor, 50); err != nil {
		t.Fatal(err)
	}
	// 80 owed, 50 pending: at most 30 more may be repaid.
	var inv *InvalidError
	if _, err := l.Repay(ctx, key(), []byte("h"), debtor, 31); !errors.As(err, &inv) {
		t.Errorf("overpay err = %v, want invalid", err)
	}
	drain(t, l)
	if b := balance(t, l, debtor); b.Posted != 0 || b.ReceivableOwed != 30 {
		t.Errorf("after repay posted=%d owed=%d, want 0/30", b.Posted, b.ReceivableOwed)
	}

	fund(t, l, debtor, 40)
	if _, err := l.Repay(ctx, key(), []byte("h"), debtor, 30); err != nil {
		t.Fatal(err)
	}
	drain(t, l)
	if b := balance(t, l, debtor); b.Posted != 10 || b.ReceivableOwed != 0 {
		t.Errorf("paid off: posted=%d owed=%d, want 10/0", b.Posted, b.ReceivableOwed)
	}
	if err := blocked["transfer"](); err != nil {
		t.Errorf("transfer after paying off: %v", err)
	}
	drain(t, l)
}

func TestRepayWithoutReceivable(t *testing.T) {
	l := testLedger(t)
	w := wallets(t, l, 1)[0]
	fund(t, l, w, 10)
	var inv *InvalidError
	if _, err := l.Repay(context.Background(), key(), []byte("h"), w, 5); !errors.As(err, &inv) {
		t.Errorf("err = %v, want invalid (nothing to repay)", err)
	}
}
