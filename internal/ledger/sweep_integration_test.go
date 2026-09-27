package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"payment-ledger/internal/store"
)

func TestSweepExpiresHoldsAndFailsTransfers(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)

	auto, err := l.CreateTransfer(ctx, AcceptRequest{
		Key: key(), Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: 30, HoldTTL: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	manual := placeHold(t, l, ws[0], ws[1], 40, time.Millisecond)
	live := placeHold(t, l, ws[0], ws[1], 10, time.Hour)
	time.Sleep(20 * time.Millisecond)

	// Lazy expiry: before any sweep, Available already ignores the expired Holds.
	if b := balance(t, l, ws[0]); b.Available != 90 {
		t.Fatalf("available before sweep = %d, want 90", b.Available)
	}

	if _, _, err := l.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	checkExpired(t, l, "auto", auto.ID)
	checkExpired(t, l, "manual", manual.TransferID)

	if h, err := store.GetHoldByTransfer(ctx, l.db, live.TransferID); err != nil || h.Status != "active" {
		t.Errorf("unexpired hold status=%s err=%v, want active", h.Status, err)
	}
	if b := balance(t, l, ws[0]); b.Posted != 100 || b.Available != 90 {
		t.Errorf("after sweep posted=%d available=%d, want 100/90", b.Posted, b.Available)
	}
}

// checkExpired asserts the Transfer's Hold is expired and the Transfer failed.
func checkExpired(t *testing.T, l *Ledger, name string, transferID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	h, err := store.GetHoldByTransfer(ctx, l.db, transferID)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := l.GetTransfer(ctx, transferID)
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "expired" || tr.Status != "failed" {
		t.Errorf("%s: hold=%s transfer=%s, want expired/failed", name, h.Status, tr.Status)
	}
}

func TestSweepPurgesOldIdempotencyKeys(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	w := wallets(t, l, 1)[0]

	oldKey, freshKey := key(), key()
	for _, k := range []string{oldKey, freshKey} {
		if _, err := l.TopUp(ctx, k, []byte("h"), w, 1); err != nil {
			t.Fatal(err)
		}
	}
	age := l.cfg.KeyRetention + time.Hour
	if _, err := l.db.Exec(ctx,
		`UPDATE idempotency_keys SET created_at = created_at - $2::bigint * interval '1 microsecond' WHERE key = $1`,
		oldKey, age.Microseconds()); err != nil {
		t.Fatal(err)
	}

	if _, _, err := l.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]int{oldKey: 0, freshKey: 1} {
		var n int
		if err := l.db.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE key = $1`, k).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("key %s: %d rows after sweep, want %d", k, n, want)
		}
	}
	drain(t, l)
}
