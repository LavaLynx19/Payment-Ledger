package ledger

import (
	"context"

	"payment-ledger/internal/store"
)

// SweepBatch is how many rows one sweep or purge statement touches.
const SweepBatch = 1000

// Sweep finalizes expired Holds, failing their Transfers, and purges
// idempotency keys older than Config.KeyRetention. It runs batches until
// both are caught up and returns the totals.
func (l *Ledger) Sweep(ctx context.Context) (expired, purged int64, err error) {
	// Each batch is its own statement, so a crash between batches (the
	// mid_batch points) leaves earlier batches committed and later ones for
	// the next sweep.
	for {
		n, err := store.ExpireHolds(ctx, l.db, SweepBatch)
		expired += n
		if err != nil {
			return expired, purged, err
		}
		if n > 0 {
			l.fail("sweeper.mid_batch")
		}
		if n < SweepBatch {
			break
		}
	}
	for {
		n, err := store.PurgeIdempotencyKeys(ctx, l.db, l.cfg.KeyRetention, SweepBatch)
		purged += n
		if err != nil {
			return expired, purged, err
		}
		if n > 0 {
			l.fail("purge.mid_batch")
		}
		if n < SweepBatch {
			return expired, purged, nil
		}
	}
}
