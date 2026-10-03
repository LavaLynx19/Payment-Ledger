package ledger

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// saga returns a copy of l that posts cross-shard credits through the outbox.
func saga(l *Ledger) *Ledger {
	c := *l
	c.cfg.CrossShard = CrossShardSaga
	return &c
}

func outboxRows(t *testing.T, l *Ledger) int {
	t.Helper()
	total := 0
	for i := range l.shards.N() {
		var n int
		if err := l.shards.Pool(i).QueryRow(context.Background(), `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	return total
}

// sagaPayment accepts and captures (without relaying) a cross-shard payment.
func sagaPayment(t *testing.T, s *Ledger, perShard []*Ledger, amounts ...int64) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	payer := walletsOn(t, perShard[0], 0, 1)[0]
	payee := walletsOn(t, perShard[1], 1, 1)[0]
	fund(t, perShard[0], payer, 1000)
	for _, a := range amounts {
		if _, err := s.CreateTransfer(ctx, AcceptRequest{Key: keyOn(s, 0), Hash: []byte("h"), SourceID: payer, DestID: payee, Amount: a}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CaptureBatch(ctx, 100); err != nil {
		t.Fatal(err)
	}
	return payer, payee
}

func TestSagaPostsDebitThenRelaysCredit(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	drainAll(t, multi)
	s := saga(multi)
	payer, payee := sagaPayment(t, s, perShard, 30)

	// Leg 1 is done: debit posted, credit waiting in the outbox.
	if b, _ := s.GetBalance(ctx, payer); b.Posted != 970 {
		t.Errorf("payer posted = %d, want 970", b.Posted)
	}
	if b, _ := s.GetBalance(ctx, payee); b.Posted != 0 {
		t.Errorf("payee posted = %d before relay, want 0 (credit in flight)", b.Posted)
	}
	if n := outboxRows(t, s); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}

	if n, err := s.Relay(ctx, 100); err != nil || n != 1 {
		t.Fatalf("relay settled %d, %v; want 1", n, err)
	}
	if n, err := s.Relay(ctx, 100); err != nil || n != 0 {
		t.Fatalf("second relay settled %d, %v; want 0", n, err)
	}
	if b, _ := s.GetBalance(ctx, payee); b.Posted != 30 {
		t.Errorf("payee posted = %d after relay, want 30 (exactly once)", b.Posted)
	}
	if n := outboxRows(t, s); n != 0 {
		t.Errorf("outbox rows = %d after relay, want 0", n)
	}
	noLeftovers(t, s)
}

// A relay that crashes after committing the credits but before deleting its
// outbox rows must not credit twice when the next relay picks the rows up.
func TestSagaRelayCrashAfterCreditAppliesOnce(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	drainAll(t, multi)
	s := saga(multi)
	_, payee := sagaPayment(t, s, perShard, 30)

	mustCrash(t, "saga.after_credit", func() { _, _ = crashing(t, s, "saga.after_credit").Relay(ctx, 100) })
	if b, _ := s.GetBalance(ctx, payee); b.Posted != 30 {
		t.Fatalf("payee posted = %d after crashed relay, want 30 (credit committed)", b.Posted)
	}
	if n := outboxRows(t, s); n != 1 {
		t.Fatalf("outbox rows = %d after crash, want 1 (delete rolled back)", n)
	}

	if n, err := s.Relay(ctx, 100); err != nil || n != 1 {
		t.Fatalf("recovery relay settled %d, %v; want 1", n, err)
	}
	if b, _ := s.GetBalance(ctx, payee); b.Posted != 30 {
		t.Errorf("payee posted = %d, want 30 (not credited twice)", b.Posted)
	}
	if n := outboxRows(t, s); n != 0 {
		t.Errorf("outbox rows = %d, want 0", n)
	}
}

func TestConcurrentRelaysApplyEachCreditOnce(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	drainAll(t, multi)
	s := saga(multi)
	amounts := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	_, payee := sagaPayment(t, s, perShard, amounts...)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				n, err := s.Relay(ctx, 3)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	if b, _ := s.GetBalance(ctx, payee); b.Posted != 55 {
		t.Errorf("payee posted = %d, want 55 (each credit once)", b.Posted)
	}
	if n := outboxRows(t, s); n != 0 {
		t.Errorf("outbox rows = %d, want 0", n)
	}
}
