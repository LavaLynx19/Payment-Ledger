package ledger

import (
	"context"
	"testing"
)

// One claimed batch on shard 0 mixes local and cross-shard destinations,
// including a hot merchant on shard 1. It must post in one atomic write:
// debits on shard 0, credits on each destination's shard, the merchant netted
// once with consecutive versions, and no 2PC leftovers.
func TestCaptureBatchMixesLocalAndCrossShard(t *testing.T) {
	multi, perShard := twoShardLedgers(t)
	ctx := context.Background()
	drainAll(t, multi) // the batch should hold only this test's Holds

	payers := walletsOn(t, perShard[0], 0, 6)
	local := walletsOn(t, perShard[0], 0, 1)[0]
	merchant := walletsOn(t, perShard[1], 1, 1)[0]
	for _, p := range payers {
		fund(t, perShard[0], p, 100)
	}

	// Five payers pay the merchant on shard 1; one pays a local Wallet.
	for i, p := range payers {
		dst := merchant
		if i == 0 {
			dst = local
		}
		if _, err := multi.CreateTransfer(ctx, AcceptRequest{Key: keyOn(multi, i%2), Hash: []byte("h"), SourceID: p, DestID: dst, Amount: int64(10 + i)}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := multi.captureBatchOn(ctx, 0, 100) // one claimed batch on shard 0
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("claimed %d holds on shard 0, want 6", n)
	}

	if b, _ := multi.GetBalance(ctx, local); b.Posted != 10 {
		t.Errorf("local payee posted = %d, want 10", b.Posted)
	}
	// Payers 1..5 paid 11..15 → 65, as one netted update with 5 consecutive versions.
	es, _, err := multi.ListEntries(ctx, merchant, nil, nil, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var running int64
	for i, e := range es {
		running += e.Amount
		if e.AccountVersion != int64(i+1) || e.BalanceAfter != running {
			t.Errorf("merchant entry %d: version=%d balance_after=%d, want %d/%d", i, e.AccountVersion, e.BalanceAfter, i+1, running)
		}
	}
	if len(es) != 5 || running != 65 {
		t.Errorf("merchant got %d entries totalling %d, want 5 totalling 65", len(es), running)
	}
	for i, p := range payers {
		if b, _ := multi.GetBalance(ctx, p); b.Posted != int64(90-i) {
			t.Errorf("payer %d posted = %d, want %d", i, b.Posted, 90-i)
		}
	}
	noLeftovers(t, multi)
}
