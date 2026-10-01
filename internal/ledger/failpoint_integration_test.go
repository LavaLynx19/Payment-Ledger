package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"payment-ledger/internal/failpoint"
	"payment-ledger/internal/store"
)

type crashSignal struct{ name string }

// crashing returns a copy of l whose named failpoint panics instead of exiting.
func crashing(t *testing.T, l *Ledger, name string) *Ledger {
	t.Helper()
	fp, err := failpoint.Parse(name)
	if err != nil {
		t.Fatal(err)
	}
	fp.WithCrash(func(n string) { panic(crashSignal{n}) })
	c := *l
	c.cfg.Failpoints = fp
	return &c
}

// mustCrash runs fn and asserts it crashed at name. The panic unwinds through
// pgx.BeginTxFunc's deferred rollback, so an open tx is rolled back, just as
// Postgres does when a killed process's connection drops.
func mustCrash(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if sig, ok := recover().(crashSignal); !ok || sig.name != name {
			t.Fatalf("expected a crash at %s", name)
		}
	}()
	fn()
}

func transferForKey(t *testing.T, l *Ledger, k string) (uuid.UUID, bool) {
	t.Helper()
	var id uuid.UUID
	err := l.db.QueryRow(context.Background(), `SELECT transfer_id FROM idempotency_keys WHERE key = $1`, k).Scan(&id)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

func TestCrashDuringAccept(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)
	req := func(k string) AcceptRequest {
		return AcceptRequest{Key: k, Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: 40}
	}

	t.Run("before commit leaves nothing", func(t *testing.T) {
		k := key()
		mustCrash(t, "accept.before_commit", func() { _, _ = crashing(t, l, "accept.before_commit").CreateTransfer(ctx, req(k)) })
		if _, ok := transferForKey(t, l, k); ok {
			t.Fatal("key persisted despite crash before commit")
		}
		if b := balance(t, l, ws[0]); b.Available != 100 {
			t.Fatalf("available = %d, want 100 (no Hold)", b.Available)
		}
		if _, err := l.CreateTransfer(ctx, req(k)); err != nil { // client retry
			t.Fatal(err)
		}
		drain(t, l)
	})

	t.Run("after commit, retry returns the same transfer", func(t *testing.T) {
		k := key()
		mustCrash(t, "accept.after_commit", func() { _, _ = crashing(t, l, "accept.after_commit").CreateTransfer(ctx, req(k)) })
		id, ok := transferForKey(t, l, k)
		if !ok {
			t.Fatal("transfer lost despite commit")
		}
		retry, err := l.CreateTransfer(ctx, req(k))
		if err != nil || retry.ID != id {
			t.Fatalf("retry = %s, %v; want %s", retry.ID, err, id)
		}
		drain(t, l)
		if b := balance(t, l, ws[0]); b.Posted != 20 {
			t.Errorf("posted = %d, want 20 (two transfers of 40, none doubled)", b.Posted)
		}
	})
}

func TestCrashDuringCapture(t *testing.T) {
	for _, point := range []string{"capture.after_claim", "capture.after_entries", "capture.after_commit"} {
		t.Run(point, func(t *testing.T) {
			l := testLedger(t)
			ctx := context.Background()
			ws := wallets(t, l, 2)
			fund(t, l, ws[0], 100)
			tr, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: 30})
			if err != nil {
				t.Fatal(err)
			}

			mustCrash(t, point, func() { _, _ = crashing(t, l, point).CaptureNext(ctx) })
			got, err := l.GetTransfer(ctx, tr.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantAfterCrash := "pending" // tx rolled back
			if point == "capture.after_commit" {
				wantAfterCrash = "posted"
			}
			if got.Status != wantAfterCrash {
				t.Fatalf("after crash status = %s, want %s", got.Status, wantAfterCrash)
			}

			drain(t, l) // a restarted worker re-claims the Hold, if it's still active
			if b := balance(t, l, ws[0]); b.Posted != 70 {
				t.Errorf("source posted = %d, want 70 (captured exactly once)", b.Posted)
			}
			if b := balance(t, l, ws[1]); b.Posted != 30 {
				t.Errorf("dest posted = %d, want 30", b.Posted)
			}
		})
	}
}

func TestCrashDuringRelease(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)
	h := placeHold(t, l, ws[0], ws[1], 50, 0)

	k := key()
	mustCrash(t, "release.before_commit", func() { _, _ = crashing(t, l, "release.before_commit").ReleaseHold(ctx, k, []byte("r"), h.ID) })
	if hold, err := store.GetHoldByTransfer(ctx, l.db, h.TransferID); err != nil || hold.Status != "active" {
		t.Fatalf("after crash hold = %s, %v; want active", hold.Status, err)
	}
	if got, err := l.ReleaseHold(ctx, k, []byte("r"), h.ID); err != nil || got.Status != "released" {
		t.Fatalf("retry = %s, %v; want released", got.Status, err)
	}
}

func TestCrashDuringReversal(t *testing.T) {
	for _, point := range []string{"reversal.before_commit", "reversal.after_commit"} {
		t.Run(point, func(t *testing.T) {
			l := testLedger(t)
			ctx := context.Background()
			payer, _, id := paid(t, l, 100)

			k := key()
			mustCrash(t, point, func() { _, _, _ = crashing(t, l, point).ReverseTransfer(ctx, k, []byte("r"), id) })
			rev, _, err := store.ReversalsOf(ctx, l.db, id)
			if err != nil {
				t.Fatal(err)
			}
			if committed := rev != nil; committed != (point == "reversal.after_commit") {
				t.Fatalf("reversal persisted = %v after crash at %s", committed, point)
			}

			retry, _, err := l.ReverseTransfer(ctx, k, []byte("r"), id)
			if err != nil || retry == nil || (rev != nil && retry.ID != rev.ID) {
				t.Fatalf("retry = %v, %v", retry, err)
			}
			drain(t, l)
			if got := balance(t, l, payer).Posted; got != 100 {
				t.Errorf("payer posted = %d, want 100 (reversed exactly once)", got)
			}
		})
	}
}

func TestCrashDuringSweep(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	if _, _, err := l.Sweep(ctx); err != nil { // clear any backlog so ours is in the first batch
		t.Fatal(err)
	}
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)

	t.Run("sweeper", func(t *testing.T) {
		h := placeHold(t, l, ws[0], ws[1], 10, time.Millisecond)
		time.Sleep(20 * time.Millisecond)
		mustCrash(t, "sweeper.mid_batch", func() { _, _, _ = crashing(t, l, "sweeper.mid_batch").Sweep(ctx) })
		checkExpired(t, l, "batch before crash", h.TransferID) // its batch committed
		if _, _, err := l.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("purge", func(t *testing.T) {
		old := key()
		if _, err := l.TopUp(ctx, old, []byte("h"), ws[0], 1); err != nil {
			t.Fatal(err)
		}
		drain(t, l)
		if _, err := l.db.Exec(ctx,
			`UPDATE idempotency_keys SET created_at = created_at - interval '2 hours' WHERE key = $1`, old); err != nil {
			t.Fatal(err)
		}
		mustCrash(t, "purge.mid_batch", func() { _, _, _ = crashing(t, l, "purge.mid_batch").Sweep(ctx) })
		if _, ok := transferForKey(t, l, old); ok {
			t.Error("old key survived; its purge batch should have committed before the crash")
		}
	})
}
