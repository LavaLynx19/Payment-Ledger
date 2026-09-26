package ledger

import (
	"context"
	"errors"
	"os"
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
	l, err := New(ctx, db, Config{HoldTTL: time.Minute, CASAttempts: 10})
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

// drain captures until no Hold is waiting.
func drain(t *testing.T, l *Ledger) {
	t.Helper()
	for {
		found, err := l.CaptureNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return
		}
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
		{"funding as p2p source", AcceptRequest{SourceID: l.fundingID, DestID: w, Amount: 1}},
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
