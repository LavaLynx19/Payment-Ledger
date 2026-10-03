package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// keyOn returns a fresh Idempotency-Key that hashes to shard i.
func keyOn(l *Ledger, i int) string {
	for {
		if k := key(); l.keyShard(k) == i {
			return k
		}
	}
}

// drainAll captures on every shard until no capturable Hold remains anywhere.
func drainAll(t *testing.T, l *Ledger) {
	t.Helper()
	ctx := context.Background()
	for range 1000 {
		n, err := l.CaptureBatch(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("drainAll: holds kept appearing")
}

// noLeftovers asserts the 2PC left nothing prepared or undecided anywhere.
func noLeftovers(t *testing.T, l *Ledger) {
	t.Helper()
	for i := range l.shards.N() {
		var prepared, decisions int
		if err := l.shards.Pool(i).QueryRow(context.Background(),
			`SELECT (SELECT count(*) FROM pg_prepared_xacts), (SELECT count(*) FROM decisions)`,
		).Scan(&prepared, &decisions); err != nil {
			t.Fatal(err)
		}
		if prepared != 0 || decisions != 0 {
			t.Errorf("shard %d: %d prepared txs and %d decisions left", i, prepared, decisions)
		}
	}
}

func onShard(t *testing.T, l *Ledger, id uuid.UUID, want int) {
	t.Helper()
	if got := l.shardOf(id); got != want {
		t.Errorf("%s lives on shard %d, want %d", id, got, want)
	}
}

func TestCrossShardTransfer(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	payer := walletsOn(t, perShard[0], 0, 1)[0]
	payee := walletsOn(t, perShard[1], 1, 1)[0]
	fund(t, perShard[0], payer, 100)

	// The key lives on shard 1, the Transfer on shard 0: a 2PC Accept.
	k := keyOn(multi, 1)
	req := AcceptRequest{Key: k, Hash: []byte("h"), SourceID: payer, DestID: payee, Amount: 30}
	tr, err := multi.CreateTransfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	onShard(t, multi, tr.ID, 0)
	if again, err := multi.CreateTransfer(ctx, req); err != nil || again.ID != tr.ID {
		t.Fatalf("replay = %s, %v; want %s", again.ID, err, tr.ID)
	}
	req.Hash = []byte("other")
	if _, err := multi.CreateTransfer(ctx, req); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Errorf("mismatch across shards: err = %v", err)
	}

	drainAll(t, multi) // the capture posts the debit on shard 0 and the credit on shard 1
	if b, _ := multi.GetBalance(ctx, payer); b.Posted != 70 {
		t.Errorf("payer posted = %d, want 70", b.Posted)
	}
	if b, _ := multi.GetBalance(ctx, payee); b.Posted != 30 {
		t.Errorf("payee posted = %d, want 30", b.Posted)
	}
	if got, _ := multi.GetTransfer(ctx, tr.ID); got.Status != "posted" {
		t.Errorf("transfer status = %s, want posted", got.Status)
	}
	noLeftovers(t, multi)
}

func TestTopUpUsesWalletShardFunding(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	w := walletsOn(t, perShard[1], 1, 1)[0]
	tr, err := multi.TopUp(ctx, keyOn(multi, 0), []byte("h"), w, 50)
	if err != nil {
		t.Fatal(err)
	}
	if tr.SourceID != multi.fundingIDs[1] {
		t.Errorf("TopUp source = %s, want shard 1's funding %s", tr.SourceID, multi.fundingIDs[1])
	}
	onShard(t, multi, tr.ID, 1)
	drainAll(t, multi)
	if b, _ := multi.GetBalance(ctx, w); b.Posted != 50 {
		t.Errorf("wallet posted = %d, want 50", b.Posted)
	}
	noLeftovers(t, multi)
}

func TestCrossShardReversal(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	payer := walletsOn(t, perShard[0], 0, 1)[0]
	recipient := walletsOn(t, perShard[1], 1, 2)
	fund(t, perShard[0], payer, 100)
	tr, err := multi.CreateTransfer(ctx, AcceptRequest{Key: keyOn(multi, 0), Hash: []byte("h"), SourceID: payer, DestID: recipient[0], Amount: 100})
	if err != nil {
		t.Fatal(err)
	}
	drainAll(t, multi)
	// The recipient spends 80 on its own shard, so 20 is recoverable.
	if _, err := multi.CreateTransfer(ctx, AcceptRequest{Key: keyOn(multi, 1), Hash: []byte("h"), SourceID: recipient[0], DestID: recipient[1], Amount: 80}); err != nil {
		t.Fatal(err)
	}
	drainAll(t, multi)

	// Up to three shards: the key's (0), the payer's (0) and the recipient's (1).
	rev, recv, err := multi.ReverseTransfer(ctx, keyOn(multi, 0), []byte("r"), tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if amountOf(rev) != 20 || amountOf(recv) != 80 {
		t.Fatalf("reversal=%d receivable=%d, want 20/80", amountOf(rev), amountOf(recv))
	}
	onShard(t, multi, rev.ID, 1)
	onShard(t, multi, recv.ID, 1)
	drainAll(t, multi)
	if b, _ := multi.GetBalance(ctx, payer); b.Posted != 100 {
		t.Errorf("payer posted = %d, want 100 (made whole)", b.Posted)
	}
	if b, _ := multi.GetBalance(ctx, recipient[0]); b.Posted != 0 || b.ReceivableOwed != 80 {
		t.Errorf("recipient posted=%d owed=%d, want 0/80", b.Posted, b.ReceivableOwed)
	}
	noLeftovers(t, multi)
}

// Rung 1 regression across shards: racers whose keys land on different
// shards still can't overspend, because the source CAS stays on one shard.
func TestCrossShardAcceptsCannotOverspend(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	for round := range 5 {
		payer := walletsOn(t, perShard[0], 0, 1)[0]
		payee := walletsOn(t, perShard[1], 1, 1)[0]
		fund(t, perShard[0], payer, 100)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for i := range 20 {
			wg.Go(func() {
				<-start
				_, err := multi.CreateTransfer(ctx, AcceptRequest{Key: keyOn(multi, i%2), Hash: []byte("h"), SourceID: payer, DestID: payee, Amount: 60})
				var ins *InsufficientFundsError
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case errors.As(err, &ins), errors.Is(err, ErrRetriesExhausted):
				default:
					t.Errorf("unexpected error: %v", err)
				}
			})
		}
		close(start)
		wg.Wait()
		if won != 1 {
			t.Fatalf("round %d: %d transfers of 60 from 100 accepted, want exactly 1", round, won)
		}
		drainAll(t, multi)
		if b, _ := multi.GetBalance(ctx, payer); b.Posted != 40 {
			t.Errorf("round %d: payer posted = %d, want 40", round, b.Posted)
		}
	}
	noLeftovers(t, multi)
}

// A retry that finds its key already claimed replays, and its tx still spans
// the key's shard and the Hold's or Transfer's shard. That replay must commit
// like any cross-shard write, not fail for lack of a home (P5.12 give-ups).
func TestCrossShardReplays(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	payer := walletsOn(t, perShard[0], 0, 1)[0]
	payee := walletsOn(t, perShard[0], 0, 1)[0]
	fund(t, perShard[0], payer, 300)
	hold := func() uuid.UUID {
		t.Helper()
		_, h, err := multi.PlaceHold(ctx, AcceptRequest{Key: keyOn(multi, 0), Hash: []byte("h"), SourceID: payer, DestID: payee, Amount: 10})
		if err != nil {
			t.Fatal(err)
		}
		return h.ID
	}

	t.Run("ReleaseHold", func(t *testing.T) {
		id, k := hold(), keyOn(multi, 1)
		for attempt := range 2 {
			if h, err := multi.ReleaseHold(ctx, k, []byte("r"), id); err != nil || h.Status != "released" {
				t.Fatalf("attempt %d: status %q, err %v", attempt, h.Status, err)
			}
		}
	})
	t.Run("CaptureHold", func(t *testing.T) {
		id, k := hold(), keyOn(multi, 1)
		for attempt := range 2 {
			if tr, _, err := multi.CaptureHold(ctx, k, []byte("c"), id, nil); err != nil || tr.Status != "posted" {
				t.Fatalf("attempt %d: status %q, err %v", attempt, tr.Status, err)
			}
		}
	})
	t.Run("ReverseTransfer", func(t *testing.T) {
		tr, err := multi.CreateTransfer(ctx, AcceptRequest{Key: keyOn(multi, 0), Hash: []byte("h"), SourceID: payer, DestID: payee, Amount: 10})
		if err != nil {
			t.Fatal(err)
		}
		drainAll(t, multi)
		k := keyOn(multi, 1)
		for attempt := range 2 {
			if rev, _, err := multi.ReverseTransfer(ctx, k, []byte("v"), tr.ID); err != nil || rev == nil {
				t.Fatalf("attempt %d: reversal %v, err %v", attempt, rev, err)
			}
		}
	})
	noLeftovers(t, multi)
}
