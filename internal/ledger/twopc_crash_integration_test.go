package ledger

import (
	"context"
	"testing"
)

// A cross-shard Transfer whose coordinator crashes at each 2PC step converges,
// after the resolver runs and the client retries with the same key, to exactly
// one Transfer with the money moved once.
func TestCrossShardTransferSurvivesCoordinatorCrash(t *testing.T) {
	for _, point := range []string{
		"twopc.after_first_prepare", "twopc.after_all_prepared",
		"twopc.after_decision", "twopc.after_first_commit",
	} {
		t.Run(point, func(t *testing.T) {
			multi, perShard := twoShardLedgers(t)
			multi.cfg.PrepareTimeout = 0 // every undecided prepared tx is stale at once
			ctx := context.Background()
			payer := walletsOn(t, perShard[0], 0, 1)[0]
			payee := walletsOn(t, perShard[1], 1, 1)[0]
			fund(t, perShard[0], payer, 100)

			k := keyOn(multi, 1) // key on shard 1, Transfer on shard 0: a 2PC
			req := AcceptRequest{Key: k, Hash: []byte("h"), SourceID: payer, DestID: payee, Amount: 30}
			mustCrash(t, point, func() { _, _ = crashing(t, multi, point).CreateTransfer(ctx, req) })

			if _, _, err := multi.Resolve(ctx); err != nil {
				t.Fatal(err)
			}
			first, err := multi.CreateTransfer(ctx, req) // client retry, same key
			if err != nil {
				t.Fatal(err)
			}
			again, err := multi.CreateTransfer(ctx, req)
			if err != nil || again.ID != first.ID {
				t.Fatalf("second retry = %s, %v; want %s", again.ID, err, first.ID)
			}
			drainAll(t, multi)
			if b, _ := multi.GetBalance(ctx, payer); b.Posted != 70 || b.Available != 70 {
				t.Errorf("payer posted=%d available=%d, want 70/70 (moved once)", b.Posted, b.Available)
			}
			if b, _ := multi.GetBalance(ctx, payee); b.Posted != 30 {
				t.Errorf("payee posted = %d, want 30", b.Posted)
			}
			if _, _, err := multi.Resolve(ctx); err != nil { // clean up abort markers
				t.Fatal(err)
			}
			noLeftovers(t, multi)
		})
	}
}
