//go:build tigerbeetle

package tbengine

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"payment-ledger/internal/api"
	"payment-ledger/internal/ledger"
)

var _ api.Engine = (*Engine)(nil)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	addr := os.Getenv("TB_ADDRESS")
	if addr == "" {
		t.Skip("TB_ADDRESS not set; skipping TigerBeetle test")
	}
	e, err := New(addr, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func wallets(t *testing.T, e *Engine, n int) []uuid.UUID {
	t.Helper()
	ids, err := e.CreateWallets(n)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func key() string { return "tb-" + uuid.NewString() }

func fund(t *testing.T, e *Engine, w uuid.UUID, amount int64) {
	t.Helper()
	if _, err := e.TopUp(context.Background(), key(), nil, w, amount); err != nil {
		t.Fatal(err)
	}
}

func balance(t *testing.T, e *Engine, id uuid.UUID) ledger.Balance {
	t.Helper()
	b, err := e.GetBalance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSinglePhaseTransfers(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	ws := wallets(t, e, 2)
	fund(t, e, ws[0], 100)

	tr, err := e.CreateTransfer(ctx, ledger.AcceptRequest{Key: key(), SourceID: ws[0], DestID: ws[1], Amount: 30})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != "posted" || tr.Type != ledger.TypeP2P {
		t.Errorf("transfer status=%s type=%s, want posted/p2p (single-phase)", tr.Status, tr.Type)
	}
	if b := balance(t, e, ws[0]); b.Posted != 70 || b.Available != 70 {
		t.Errorf("payer posted=%d available=%d, want 70/70", b.Posted, b.Available)
	}
	if b := balance(t, e, ws[1]); b.Posted != 30 {
		t.Errorf("payee posted=%d, want 30", b.Posted)
	}

	var ins *ledger.InsufficientFundsError
	if _, err := e.CreateTransfer(ctx, ledger.AcceptRequest{Key: key(), SourceID: ws[0], DestID: ws[1], Amount: 71}); !errors.As(err, &ins) || ins.Available != 70 {
		t.Errorf("overdraw err = %v, want insufficient funds with available 70", err)
	}
	var nf *ledger.NotFoundError
	if _, err := e.CreateTransfer(ctx, ledger.AcceptRequest{Key: key(), SourceID: ws[0], DestID: uuid.New(), Amount: 1}); !errors.As(err, &nf) {
		t.Errorf("missing dest err = %v, want not found", err)
	}
}

func TestIdempotencyIsTheTransferID(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	ws := wallets(t, e, 2)
	fund(t, e, ws[0], 100)

	k := key()
	req := ledger.AcceptRequest{Key: k, SourceID: ws[0], DestID: ws[1], Amount: 30}
	first, err := e.CreateTransfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := e.CreateTransfer(ctx, req); err != nil || again.ID != first.ID {
		t.Fatalf("replay = %s, %v; want %s", again.ID, err, first.ID)
	}
	if b := balance(t, e, ws[0]); b.Posted != 70 {
		t.Errorf("payer posted = %d after replay, want 70 (moved once)", b.Posted)
	}
	req.Amount = 31
	if _, err := e.CreateTransfer(ctx, req); !errors.Is(err, ledger.ErrIdempotencyMismatch) {
		t.Errorf("changed amount err = %v, want mismatch", err)
	}

	// Finding (A§9.7): a key whose transfer failed for lack of funds can't
	// succeed on retry, even after the account is topped up.
	failing := ledger.AcceptRequest{Key: key(), SourceID: ws[0], DestID: ws[1], Amount: 500}
	var ins *ledger.InsufficientFundsError
	if _, err := e.CreateTransfer(ctx, failing); !errors.As(err, &ins) {
		t.Fatalf("first attempt err = %v, want insufficient funds", err)
	}
	fund(t, e, ws[0], 1000)
	var inv *ledger.InvalidError
	if _, err := e.CreateTransfer(ctx, failing); !errors.As(err, &inv) {
		t.Errorf("retry after top-up err = %v, want 'key used by a failed request'", err)
	}
}

func TestHolds(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	ws := wallets(t, e, 2)
	fund(t, e, ws[0], 100)

	t.Run("partial capture restores the remainder", func(t *testing.T) {
		tr, h, err := e.PlaceHold(ctx, ledger.AcceptRequest{Key: key(), SourceID: ws[0], DestID: ws[1], Amount: 50})
		if err != nil {
			t.Fatal(err)
		}
		if tr.Status != "pending" || h.Status != "active" {
			t.Fatalf("placed: transfer=%s hold=%s, want pending/active", tr.Status, h.Status)
		}
		if b := balance(t, e, ws[0]); b.Available != 50 || len(b.ActiveHolds) != 1 {
			t.Errorf("after hold available=%d holds=%d, want 50/1", b.Available, len(b.ActiveHolds))
		}
		amt := int64(20)
		tr, h, err = e.CaptureHold(ctx, key(), nil, h.ID, &amt)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Status != "posted" || h.Status != "captured" || *h.CapturedAmount != 20 {
			t.Errorf("captured: transfer=%s hold=%s amount=%v", tr.Status, h.Status, h.CapturedAmount)
		}
		if b := balance(t, e, ws[0]); b.Posted != 80 || b.Available != 80 {
			t.Errorf("payer posted=%d available=%d, want 80/80", b.Posted, b.Available)
		}
		var na *ledger.HoldNotActiveError
		if _, err := e.ReleaseHold(ctx, key(), nil, h.ID); !errors.As(err, &na) {
			t.Errorf("release after capture err = %v, want hold not active", err)
		}
	})

	t.Run("release", func(t *testing.T) {
		_, h, err := e.PlaceHold(ctx, ledger.AcceptRequest{Key: key(), SourceID: ws[0], DestID: ws[1], Amount: 10})
		if err != nil {
			t.Fatal(err)
		}
		if h, err = e.ReleaseHold(ctx, key(), nil, h.ID); err != nil || h.Status != "released" {
			t.Fatalf("release = %s, %v", h.Status, err)
		}
		if b := balance(t, e, ws[0]); b.Available != 80 {
			t.Errorf("available = %d after release, want 80", b.Available)
		}
	})

	t.Run("expiry", func(t *testing.T) {
		_, h, err := e.PlaceHold(ctx, ledger.AcceptRequest{Key: key(), SourceID: ws[0], DestID: ws[1], Amount: 10, HoldTTL: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2500 * time.Millisecond) // TigerBeetle's timeout has whole-second granularity
		var exp *ledger.HoldExpiredError
		if _, _, err := e.CaptureHold(ctx, key(), nil, h.ID, nil); !errors.As(err, &exp) {
			t.Errorf("capture after expiry err = %v, want hold expired", err)
		}
		if b := balance(t, e, ws[0]); b.Available != 80 {
			t.Errorf("available = %d after expiry, want 80", b.Available)
		}
	})
}

func TestHistoryReads(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	ws := wallets(t, e, 2)
	before := time.Now()
	time.Sleep(5 * time.Millisecond)
	fund(t, e, ws[0], 100)
	time.Sleep(5 * time.Millisecond)
	afterFund := time.Now()
	if _, err := e.CreateTransfer(ctx, ledger.AcceptRequest{Key: key(), SourceID: ws[0], DestID: ws[1], Amount: 30}); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		at   time.Time
		want int64
	}{"before": {before, 0}, "after top-up": {afterFund, 100}, "now": {time.Now().Add(time.Second), 70}} {
		if got, err := e.GetBalanceAt(ctx, ws[0], c.at); err != nil || got != c.want {
			t.Errorf("%s: balance = %d, %v; want %d", name, got, err, c.want)
		}
	}

	page, more, err := e.ListEntries(ctx, ws[0], nil, nil, 0, 1)
	if err != nil || len(page) != 1 || !more || page[0].Direction != "credit" || page[0].BalanceAfter != 100 {
		t.Fatalf("page 1 = %+v more=%v err=%v", page, more, err)
	}
	page, more, err = e.ListEntries(ctx, ws[0], nil, nil, page[0].AccountVersion, 1)
	if err != nil || len(page) != 1 || more || page[0].Direction != "debit" || page[0].BalanceAfter != 70 {
		t.Fatalf("page 2 = %+v more=%v err=%v", page, more, err)
	}
}
