package ledger

import (
	"context"
	"expvar"
	"sync"
	"testing"
)

func casCount(m *expvar.Map, key string) int64 {
	if v, ok := m.Get(key).(*expvar.Int); ok {
		return v.Value()
	}
	return 0
}

// Rung 3 regression: many concurrent captures paying one hot destination
// queue on its row lock and never conflict, and the money still adds up.
func TestHotDestinationCapturesDoNotConflict(t *testing.T) {
	base := testLedger(t)
	ctx := context.Background()
	payers := wallets(t, base, 20)
	merchant := wallets(t, base, 1)[0]
	for _, p := range payers {
		fund(t, base, p, 100)
	}

	stats := new(expvar.Map).Init()
	l := *base
	l.cfg.CASStats = stats
	for _, p := range payers {
		if _, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: p, DestID: merchant, Amount: 10}); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for range 8 { // eight capture loops racing on the merchant row
		wg.Go(func() {
			for {
				found, err := l.CaptureNext(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if !found {
					return
				}
			}
		})
	}
	wg.Wait()
	drain(t, base)

	if got := casCount(stats, "capture.conflicts"); got != 0 {
		t.Errorf("capture conflicts = %d, want 0 (lock-based posting)", got)
	}
	if got := balance(t, base, merchant).Posted; got != 200 {
		t.Errorf("merchant posted = %d, want 200", got)
	}
}

// Rung 3 regression: concurrent TopUps all debit the funding account, which
// has no funds check, so their Accepts never conflict on it.
func TestConcurrentTopUpsDoNotConflict(t *testing.T) {
	base := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, base, 20)

	stats := new(expvar.Map).Init()
	l := *base
	l.cfg.CASStats = stats
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Go(func() {
			if _, err := l.TopUp(ctx, key(), []byte("h"), w, 5); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	drain(t, base)

	if got := casCount(stats, "accept.conflicts"); got != 0 {
		t.Errorf("accept conflicts = %d, want 0 (no CAS on System sources)", got)
	}
	for _, w := range ws {
		if got := balance(t, base, w).Posted; got != 5 {
			t.Errorf("wallet posted = %d, want 5", got)
		}
	}
}
