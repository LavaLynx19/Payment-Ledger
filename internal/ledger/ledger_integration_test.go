package ledger

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"payment-ledger/internal/store"
)

func testLedger(t *testing.T) *Ledger {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	if _, err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := store.EnsureFundingAccount(ctx, db); err != nil {
		t.Fatal(err)
	}
	l, err := New(ctx, store.NewShards(db), Config{HoldTTL: time.Minute, CASAttempts: 10, KeyRetention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func wallets(t *testing.T, l *Ledger, n int) []uuid.UUID {
	t.Helper()
	ids, err := store.CreateWallets(context.Background(), l.db, n)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func key() string { return "test-" + uuid.NewString() }

// drain captures until no capturable (active, unexpired, auto) Hold remains.
// CaptureNext skips Holds locked by another capturer (SKIP LOCKED), so
// "nothing claimed" alone doesn't mean the other capturer's work has
// committed. Expired Holds are ignored (only the sweeper finalizes them), and
// so are manual Holds (only their caller does).
func drain(t *testing.T, l *Ledger) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		found, err := l.CaptureNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			continue
		}
		var active int
		if err := l.db.QueryRow(ctx,
			`SELECT count(*) FROM holds
			 WHERE status = 'active' AND capture_mode = 'auto'
			   AND (expires_at IS NULL OR expires_at > clock_timestamp())`,
		).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain: %d holds still active", active)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func balance(t *testing.T, l *Ledger, id uuid.UUID) Balance {
	t.Helper()
	b, err := l.GetBalance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fund(t *testing.T, l *Ledger, wallet uuid.UUID, amount int64) {
	t.Helper()
	if _, err := l.TopUp(context.Background(), key(), []byte("h"), wallet, amount); err != nil {
		t.Fatal(err)
	}
	drain(t, l)
}

func TestTopUpPostsAfterCapture(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	w := wallets(t, l, 1)[0]

	tr, err := l.TopUp(ctx, key(), []byte("h"), w, 100)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != "pending" {
		t.Fatalf("status = %s, want pending", tr.Status)
	}
	if b := balance(t, l, w); b.Posted != 0 {
		t.Fatalf("posted before capture = %d, want 0", b.Posted)
	}

	drain(t, l)
	got, err := l.GetTransfer(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "posted" || got.PostedAt == nil {
		t.Errorf("after capture: status=%s posted_at=%v", got.Status, got.PostedAt)
	}
	if b := balance(t, l, w); b.Posted != 100 || b.Available != 100 {
		t.Errorf("wallet posted=%d available=%d, want 100/100", b.Posted, b.Available)
	}
}

func TestP2PReservesAtAcceptAndPostsOnCapture(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	a, b := ws[0], ws[1]
	fund(t, l, a, 100)

	if _, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: a, DestID: b, Amount: 60}); err != nil {
		t.Fatal(err)
	}
	if got := balance(t, l, a); got.Posted != 100 || got.Available != 40 || len(got.ActiveHolds) != 1 {
		t.Fatalf("after accept: posted=%d available=%d holds=%d, want 100/40/1", got.Posted, got.Available, len(got.ActiveHolds))
	}

	_, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: a, DestID: b, Amount: 50})
	var insufficient *InsufficientFundsError
	if !errors.As(err, &insufficient) || insufficient.Available != 40 || insufficient.Amount != 50 {
		t.Fatalf("second transfer err = %v, want insufficient 40 < 50", err)
	}

	drain(t, l)
	if got := balance(t, l, a); got.Posted != 40 || got.Available != 40 {
		t.Errorf("source posted=%d available=%d, want 40/40", got.Posted, got.Available)
	}
	if got := balance(t, l, b); got.Posted != 60 {
		t.Errorf("dest posted=%d, want 60", got.Posted)
	}
}

// Rung 1 regression: concurrent Accepts that each fit the balance but not
// together. Exactly one may win. A single round let the naive Accept through
// only ~40% of the time, so the test runs several rounds from a start barrier.
func TestConcurrentAcceptsCannotOverspend(t *testing.T) {
	l := testLedger(t)
	for round := range 10 {
		raceRound(t, l, round)
	}
}

func raceRound(t *testing.T, l *Ledger, round int) {
	t.Helper()
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)

	const racers = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var won, insufficient, exhausted int
	for range racers {
		wg.Go(func() {
			<-start
			_, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: 60})
			var ins *InsufficientFundsError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.As(err, &ins):
				insufficient++
			case errors.Is(err, ErrRetriesExhausted):
				exhausted++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()

	if won != 1 {
		t.Fatalf("round %d: accepted %d transfers of 60 from 100, want exactly 1 (insufficient=%d exhausted=%d)",
			round, won, insufficient, exhausted)
	}
	drain(t, l)
	if got := balance(t, l, ws[0]); got.Posted != 40 {
		t.Errorf("round %d: source posted = %d, want 40", round, got.Posted)
	}
}

func TestAcceptIsIdempotent(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)

	k := key()
	req := AcceptRequest{Key: k, Hash: []byte("h1"), SourceID: ws[0], DestID: ws[1], Amount: 30}
	first, err := l.CreateTransfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := l.CreateTransfer(ctx, req)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("retry = %s, %v; want %s", retry.ID, err, first.ID)
	}
	if got := balance(t, l, ws[0]); got.Available != 70 {
		t.Errorf("available = %d after retry, want 70 (reserved once)", got.Available)
	}

	req.Hash = []byte("h2")
	if _, err := l.CreateTransfer(ctx, req); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Errorf("mismatch err = %v, want ErrIdempotencyMismatch", err)
	}
}

func TestWithdraw(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	w := wallets(t, l, 1)[0]
	fund(t, l, w, 100)
	fundingBefore := balance(t, l, l.fundingIDs[0]).Posted

	if _, err := l.Withdraw(ctx, key(), []byte("h"), w, 70); err != nil {
		t.Fatal(err)
	}
	var insufficient *InsufficientFundsError
	if _, err := l.Withdraw(ctx, key(), []byte("h"), w, 31); !errors.As(err, &insufficient) {
		t.Errorf("overdraw err = %v, want insufficient funds", err)
	}
	drain(t, l)
	if got := balance(t, l, w).Posted; got != 30 {
		t.Errorf("wallet posted = %d, want 30", got)
	}
	// Funding is debit-normal (simulated cash at bank), so a Withdrawal credits it down.
	if got := balance(t, l, l.fundingIDs[0]).Posted; got != fundingBefore-70 {
		t.Errorf("funding posted = %d, want %d", got, fundingBefore-70)
	}

	var inv *InvalidError
	if _, err := l.Withdraw(ctx, key(), []byte("h"), l.fundingIDs[0], 1); !errors.As(err, &inv) {
		t.Errorf("withdraw from funding err = %v, want InvalidError", err)
	}
}

func TestAcceptValidation(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	w := wallets(t, l, 1)[0]
	missing := uuid.Must(uuid.NewV7())

	cases := []struct {
		name string
		req  AcceptRequest
	}{
		{"zero amount", AcceptRequest{SourceID: w, DestID: missing, Amount: 0}},
		{"same account", AcceptRequest{SourceID: w, DestID: w, Amount: 1}},
		{"funding as p2p source", AcceptRequest{SourceID: l.fundingIDs[0], DestID: w, Amount: 1}},
	}
	for _, c := range cases {
		c.req.Key, c.req.Hash = key(), []byte("h")
		var inv *InvalidError
		if _, err := l.CreateTransfer(ctx, c.req); !errors.As(err, &inv) {
			t.Errorf("%s: err = %v, want InvalidError", c.name, err)
		}
	}

	var nf *NotFoundError
	_, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: w, DestID: missing, Amount: 1})
	if !errors.As(err, &nf) {
		t.Errorf("missing dest: err = %v, want NotFoundError", err)
	}
}
