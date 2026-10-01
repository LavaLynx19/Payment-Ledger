package ledger

import (
	"context"
	"testing"
	"time"
)

// One batch nets many captures onto a hot merchant row: every Entry still
// gets a consecutive version and a running balance_after, a Hold whose expiry
// won fails on its own, and the totals add up.
func TestCaptureBatchNetsHotAccount(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	drain(t, l) // the batch should hold only this test's Holds
	payers := wallets(t, l, 10)
	merchant := wallets(t, l, 1)[0]
	for _, p := range payers {
		fund(t, l, p, 100)
	}
	before := version(t, l, merchant)

	for i, p := range payers {
		ttl := time.Duration(0)
		if i == 0 {
			ttl = time.Millisecond // this one expires before capture
		}
		if _, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: p, DestID: merchant, Amount: int64(10 + i), HoldTTL: ttl}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)

	n, err := l.CaptureBatch(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 {
		t.Fatalf("claimed %d holds, want 9 (the expired one isn't capturable)", n)
	}

	// Merchant Entries: consecutive versions from before+1, running balances.
	rows, err := l.db.Query(ctx,
		`SELECT amount, balance_after, account_version FROM entries WHERE account_id = $1 ORDER BY account_version`, merchant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var running, wantVersion int64 = 0, before
	count := 0
	for rows.Next() {
		var amount, bal, v int64
		if err := rows.Scan(&amount, &bal, &v); err != nil {
			t.Fatal(err)
		}
		running += amount
		wantVersion++
		count++
		if v != wantVersion || bal != running {
			t.Errorf("entry %d: version=%d balance_after=%d, want %d/%d", count, v, bal, wantVersion, running)
		}
	}
	// Payers 1..9 paid 11..19 → 135.
	if count != 9 || running != 135 {
		t.Errorf("merchant got %d entries totalling %d, want 9 totalling 135", count, running)
	}
	if got := balance(t, l, merchant); got.Posted != 135 || version(t, l, merchant) != before+9 {
		t.Errorf("merchant posted=%d version=%d, want 135/%d", got.Posted, version(t, l, merchant), before+9)
	}
	if got := balance(t, l, payers[0]); got.Posted != 100 || got.Available != 100 {
		t.Errorf("expired payer posted=%d available=%d, want 100/100", got.Posted, got.Available)
	}
}
