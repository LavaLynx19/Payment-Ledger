package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"payment-ledger/internal/store"
)

func placeHold(t *testing.T, l *Ledger, src, dst uuid.UUID, amount int64, ttl time.Duration) store.Hold {
	t.Helper()
	_, h, err := l.PlaceHold(context.Background(), AcceptRequest{
		Key: key(), Hash: []byte("h"), SourceID: src, DestID: dst, Amount: amount, HoldTTL: ttl,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func ptr(n int64) *int64 { return &n }

func TestPlaceHoldIsNotCapturedByWorker(t *testing.T) {
	l := testLedger(t)
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)

	h := placeHold(t, l, ws[0], ws[1], 70, 0)
	if h.CaptureMode != CaptureManual || h.Status != "active" {
		t.Fatalf("hold mode=%s status=%s, want manual/active", h.CaptureMode, h.Status)
	}
	drain(t, l)
	if b := balance(t, l, ws[0]); b.Posted != 100 || b.Available != 30 {
		t.Errorf("after drain posted=%d available=%d, want 100/30 (hold untouched)", b.Posted, b.Available)
	}
}

func TestPartialCaptureReleasesRemainder(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)
	h := placeHold(t, l, ws[0], ws[1], 100, 0)

	k := key()
	tr, got, err := l.CaptureHold(ctx, k, []byte("c"), h.ID, ptr(60))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != "posted" || tr.Amount != 100 {
		t.Errorf("transfer status=%s amount=%d, want posted/100 (requested amount kept)", tr.Status, tr.Amount)
	}
	if got.Status != "captured" || got.CapturedAmount == nil || *got.CapturedAmount != 60 {
		t.Errorf("hold status=%s captured=%v, want captured/60", got.Status, got.CapturedAmount)
	}
	if b := balance(t, l, ws[0]); b.Posted != 40 || b.Available != 40 {
		t.Errorf("source posted=%d available=%d, want 40/40 (remainder released)", b.Posted, b.Available)
	}
	if b := balance(t, l, ws[1]); b.Posted != 60 {
		t.Errorf("dest posted=%d, want 60", b.Posted)
	}

	// Replaying the same key returns the result without posting again.
	if tr2, _, err := l.CaptureHold(ctx, k, []byte("c"), h.ID, ptr(60)); err != nil || tr2.ID != tr.ID {
		t.Fatalf("replay = %v, %v", tr2.ID, err)
	}
	if b := balance(t, l, ws[0]); b.Posted != 40 {
		t.Errorf("after replay source posted=%d, want 40", b.Posted)
	}

	var notActive *HoldNotActiveError
	if _, _, err := l.CaptureHold(ctx, key(), []byte("c"), h.ID, nil); !errors.As(err, &notActive) || notActive.Status != "captured" {
		t.Errorf("second capture err = %v, want hold not active (captured)", err)
	}
}

func TestReleaseHold(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)
	h := placeHold(t, l, ws[0], ws[1], 50, 0)

	got, err := l.ReleaseHold(ctx, key(), []byte("r"), h.ID)
	if err != nil || got.Status != "released" {
		t.Fatalf("release = %s, %v", got.Status, err)
	}
	if b := balance(t, l, ws[0]); b.Posted != 100 || b.Available != 100 {
		t.Errorf("posted=%d available=%d, want 100/100", b.Posted, b.Available)
	}
	tr, err := l.GetTransfer(ctx, h.TransferID)
	if err != nil || tr.Status != "failed" {
		t.Errorf("transfer status=%s err=%v, want failed", tr.Status, err)
	}

	var notActive *HoldNotActiveError
	if _, _, err := l.CaptureHold(ctx, key(), []byte("c"), h.ID, nil); !errors.As(err, &notActive) {
		t.Errorf("capture after release err = %v, want hold not active", err)
	}
}

func TestCaptureExpiredHold(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)
	h := placeHold(t, l, ws[0], ws[1], 80, time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	var expired *HoldExpiredError
	if _, _, err := l.CaptureHold(ctx, key(), []byte("c"), h.ID, nil); !errors.As(err, &expired) {
		t.Fatalf("err = %v, want hold expired", err)
	}
	if b := balance(t, l, ws[0]); b.Available != 100 {
		t.Errorf("available = %d, want 100 (expired Hold no longer reserves)", b.Available)
	}
}

func TestCaptureHoldRejects(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	ws := wallets(t, l, 2)
	fund(t, l, ws[0], 100)

	auto, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: 10})
	if err != nil {
		t.Fatal(err)
	}
	autoHold, err := store.GetHoldByTransfer(ctx, l.db, auto.ID)
	if err != nil {
		t.Fatal(err)
	}
	manual := placeHold(t, l, ws[0], ws[1], 50, 0)

	var inv *InvalidError
	var nf *NotFoundError
	cases := []struct {
		name   string
		holdID uuid.UUID
		amount *int64
		target any
	}{
		{"auto hold", autoHold.ID, nil, &inv},
		{"zero amount", manual.ID, ptr(0), &inv},
		{"over hold amount", manual.ID, ptr(51), &inv},
		{"missing hold", uuid.Must(uuid.NewV7()), nil, &nf},
	}
	for _, c := range cases {
		_, _, err := l.CaptureHold(ctx, key(), []byte("c"), c.holdID, c.amount)
		if !errors.As(err, c.target) {
			t.Errorf("%s: err = %v, want %T", c.name, err, c.target)
		}
	}
	if _, err := l.ReleaseHold(ctx, key(), []byte("r"), autoHold.ID); !errors.As(err, &inv) {
		t.Errorf("release auto hold err = %v, want InvalidError", err)
	}
}
