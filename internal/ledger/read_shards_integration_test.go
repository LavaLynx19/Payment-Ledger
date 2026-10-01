package ledger

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"payment-ledger/internal/store"
)

// twoShardLedgers returns a Ledger over every shard in SHARD_URLS (for
// reads) and one single-shard Ledger per shard to write with: until
// cross-shard writes land (P5.5–P5.6), each shard's data comes from a Ledger
// that only sees that shard. Skips unless SHARD_URLS lists at least two.
func twoShardLedgers(t *testing.T) (*Ledger, []*Ledger) {
	t.Helper()
	var urls []string
	for _, u := range strings.Split(os.Getenv("SHARD_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) < 2 {
		t.Skip("SHARD_URLS lists fewer than 2 shards; skipping multi-shard test")
	}
	ctx := context.Background()
	if err := store.MigrateAll(ctx, urls); err != nil {
		t.Fatal(err)
	}
	cfg := Config{HoldTTL: time.Minute, CASAttempts: 10, KeyRetention: time.Hour}
	var pools []*pgxpool.Pool
	var perShard []*Ledger
	for i, u := range urls {
		pool, err := store.Open(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		if _, err := store.EnsureFundingAccountOn(ctx, pool, i); err != nil {
			t.Fatal(err)
		}
		l, err := New(ctx, store.NewShards(pool), cfg)
		if err != nil {
			t.Fatal(err)
		}
		pools = append(pools, pool)
		perShard = append(perShard, l)
	}
	multi, err := New(ctx, store.NewShards(pools...), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return multi, perShard
}

// walletsOn creates n Wallets on shard i, minted with i's bits.
func walletsOn(t *testing.T, l *Ledger, i, n int) []uuid.UUID {
	t.Helper()
	ids, err := store.CreateWalletsOn(context.Background(), l.db, i, n)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestReadsRouteAcrossShards(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	for i, l := range perShard {
		ws := walletsOn(t, l, i, 2)
		fund(t, l, ws[0], int64(100+i))
		tr, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: ws[0], DestID: ws[1], Amount: 30})
		if err != nil {
			t.Fatal(err)
		}
		drain(t, l)
		at := dbNow(t, l)

		want := int64(70 + i)
		if b, err := multi.GetBalance(ctx, ws[0]); err != nil || b.Posted != want {
			t.Errorf("shard %d GetBalance = %d, %v; want %d", i, b.Posted, err, want)
		}
		if got, err := multi.GetTransfer(ctx, tr.ID); err != nil || got.Status != "posted" {
			t.Errorf("shard %d GetTransfer = %s, %v; want posted", i, got.Status, err)
		}
		if got, err := multi.GetBalanceAt(ctx, ws[0], at); err != nil || got != want {
			t.Errorf("shard %d GetBalanceAt = %d, %v; want %d", i, got, err, want)
		}
		if es, _, err := multi.ListEntries(ctx, ws[0], nil, nil, 0, 10); err != nil || len(es) != 2 {
			t.Errorf("shard %d ListEntries = %d entries, %v; want 2", i, len(es), err)
		}
	}
}

func TestListReceivablesFansOutAcrossShards(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()

	// One debtor per shard: paid 100, spent it all, then reversed, so it owes 100.
	debtors := map[uuid.UUID]bool{}
	for i, l := range perShard {
		ws := walletsOn(t, l, i, 3)
		payer, debtor, sink := ws[0], ws[1], ws[2]
		fund(t, l, payer, 100)
		tr, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: payer, DestID: debtor, Amount: 100})
		if err != nil {
			t.Fatal(err)
		}
		drain(t, l)
		if _, err := l.CreateTransfer(ctx, AcceptRequest{Key: key(), Hash: []byte("h"), SourceID: debtor, DestID: sink, Amount: 100}); err != nil {
			t.Fatal(err)
		}
		drain(t, l)
		if _, _, err := l.ReverseTransfer(ctx, key(), []byte("r"), tr.ID); err != nil {
			t.Fatal(err)
		}
		debtors[debtor] = false
	}

	// Page one row at a time across both shards.
	var prev uuid.UUID
	seen := map[uuid.UUID]bool{}
	after := uuid.Nil
	for pages := 0; ; pages++ {
		if pages > 100000 {
			t.Fatal("pagination never ended")
		}
		page, more, err := multi.ListReceivables(ctx, 0, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			if seen[r.DebtorWalletID] {
				t.Fatalf("debtor %s listed twice", r.DebtorWalletID)
			}
			if prev != uuid.Nil && bytes.Compare(prev[:], r.DebtorWalletID[:]) >= 0 {
				t.Fatalf("out of order: %s after %s", r.DebtorWalletID, prev)
			}
			seen[r.DebtorWalletID], prev = true, r.DebtorWalletID
			if _, ok := debtors[r.DebtorWalletID]; ok {
				debtors[r.DebtorWalletID] = r.Owed == 100
			}
		}
		if !more {
			break
		}
		after = page[len(page)-1].DebtorWalletID
	}
	for d, ok := range debtors {
		if !ok {
			t.Errorf("debtor %s (shard %d) missing or not owing 100", d, multi.shardOf(d))
		}
	}
}
