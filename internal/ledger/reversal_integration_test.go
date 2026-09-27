package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"payment-ledger/internal/store"
)

// paid runs a posted P2P Transfer of amount from a funded payer to a fresh
// recipient and returns (payer, recipient, transfer id).
func paid(t *testing.T, l *Ledger, amount int64) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], amount)
	tr, err := l.CreateTransfer(context.Background(), AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: amount})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, l)
	return ws[0], ws[1], tr.ID
}

// spend moves amount out of wallet to a throwaway Wallet and posts it.
func spend(t *testing.T, l *Ledger, wallet uuid.UUID, amount int64) {
	t.Helper()
	sink := wallets(t, l, 1)[0]
	if _, err := l.CreateTransfer(context.Background(), AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: wallet, DestID: sink, Amount: amount}); err != nil {
		t.Fatal(err)
	}
	drain(t, l)
}

func reverse(t *testing.T, l *Ledger, id uuid.UUID) (*store.Transfer, *store.Transfer) {
	t.Helper()
	rev, recv, err := l.ReverseTransfer(context.Background(), key(), []byte("r"), id)
	if err != nil {
		t.Fatal(err)
	}
	return rev, recv
}

func amountOf(tr *store.Transfer) int64 {
	if tr == nil {
		return 0
	}
	return tr.Amount
}

func TestReversal(t *testing.T) {
	cases := []struct {
		name                string
		spent               int64 // recipient spends this before the reversal
		wantRev, wantRecv   int64
		wantPayer, wantDebt int64 // after capture
	}{
		{"fully recoverable", 0, 100, 0, 100, 0},
		{"partly spent", 80, 20, 80, 100, 80},
		{"fully spent", 100, 0, 100, 100, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := testLedger(t)
			payer, recipient, id := paid(t, l, 100)
			if c.spent > 0 {
				spend(t, l, recipient, c.spent)
			}
			before := version(t, l, recipient)

			rev, recv := reverse(t, l, id)
			if amountOf(rev) != c.wantRev || amountOf(recv) != c.wantRecv {
				t.Fatalf("reversal=%d receivable=%d, want %d/%d", amountOf(rev), amountOf(recv), c.wantRev, c.wantRecv)
			}
			// The Receivable is open from Accept, before any capture.
			if got := balance(t, l, recipient).ReceivableOwed; got != c.wantRecv {
				t.Errorf("owed before capture = %d, want %d", got, c.wantRecv)
			}
			if after := version(t, l, recipient); after <= before {
				t.Errorf("recipient version %d → %d, want a bump even when nothing is recoverable", before, after)
			}

			drain(t, l)
			if got := balance(t, l, payer).Posted; got != c.wantPayer {
				t.Errorf("payer posted = %d, want %d (made whole)", got, c.wantPayer)
			}
			rb := balance(t, l, recipient)
			if rb.Posted != 0 || rb.ReceivableOwed != c.wantDebt {
				t.Errorf("recipient posted=%d owed=%d, want 0/%d", rb.Posted, rb.ReceivableOwed, c.wantDebt)
			}
		})
	}
}

func TestReversalReplayAndRejections(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	_, _, id := paid(t, l, 100)

	k := key()
	rev, _, err := l.ReverseTransfer(ctx, k, []byte("r"), id)
	if err != nil {
		t.Fatal(err)
	}
	if again, _, err := l.ReverseTransfer(ctx, k, []byte("r"), id); err != nil || again == nil || again.ID != rev.ID {
		t.Fatalf("replay = %v, %v; want %s", again, err, rev.ID)
	}

	var nr *NotReversibleError
	if _, _, err := l.ReverseTransfer(ctx, key(), []byte("r"), id); !errors.As(err, &nr) {
		t.Errorf("second reversal err = %v, want not reversible", err)
	}
	if _, _, err := l.ReverseTransfer(ctx, key(), []byte("r"), rev.ID); !errors.As(err, &nr) {
		t.Errorf("reversing a reversal err = %v, want not reversible", err)
	}

	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 10)
	pending, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.ReverseTransfer(ctx, key(), []byte("r"), pending.ID); !errors.As(err, &nr) {
		t.Errorf("reversing a pending transfer err = %v, want not reversible", err)
	}
	drain(t, l)

	var nf *NotFoundError
	if _, _, err := l.ReverseTransfer(ctx, key(), []byte("r"), uuid.Must(uuid.NewV7())); !errors.As(err, &nf) {
		t.Errorf("missing transfer err = %v, want not found", err)
	}
}

func TestReversalOfPartialCaptureUsesPostedAmount(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)
	h := placeHold(t, l, ws[0], ws[1], 100, 0)
	if _, _, err := l.CaptureHold(ctx, key(), []byte("c"), h.ID, ptr(60)); err != nil {
		t.Fatal(err)
	}

	rev, recv := reverse(t, l, h.TransferID)
	if amountOf(rev) != 60 || recv != nil {
		t.Errorf("reversal=%d receivable=%v, want 60/nil (posted, not requested, amount)", amountOf(rev), recv)
	}
	drain(t, l)
	if got := balance(t, l, ws[0]).Posted; got != 100 {
		t.Errorf("payer posted = %d, want 100", got)
	}
}

func version(t *testing.T, l *Ledger, id uuid.UUID) int64 {
	t.Helper()
	var v int64
	if err := l.db.QueryRow(context.Background(), `SELECT version FROM accounts WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
