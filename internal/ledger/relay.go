package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

// Relay applies saga credits waiting in every shard's outbox (A§9.5). It
// returns how many outbox rows it settled, applied now or found already
// applied by an earlier, crashed relay.
func (l *Ledger) Relay(ctx context.Context, max int) (int, error) {
	total := 0
	for i := range l.shards.N() {
		n, err := l.relayOn(ctx, i, max)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// relayOn claims outbox rows on source shard i, applies their credits on each
// destination shard in one local tx per shard, then deletes the rows. A crash
// after the credits commit leaves the rows; the next relay finds those
// credits already applied and only deletes them.
func (l *Ledger) relayOn(ctx context.Context, i, max int) (int, error) {
	var settled int
	err := pgx.BeginTxFunc(ctx, l.shards.Pool(i), pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(src pgx.Tx) error {
		rows, err := store.ClaimOutbox(ctx, src, max)
		if err != nil || len(rows) == 0 {
			return err
		}
		byShard := map[int][]store.OutboxRow{}
		ids := make([]uuid.UUID, len(rows))
		for k, r := range rows {
			d := l.shardOf(r.DestID)
			byShard[d] = append(byShard[d], r)
			ids[k] = r.TransferID
		}
		for d, part := range byShard {
			if err := l.applyCredits(ctx, d, part); err != nil {
				return fmt.Errorf("relay to shard %d: %w", d, err)
			}
		}
		l.fail("saga.after_credit")
		if err := store.DeleteOutbox(ctx, src, ids); err != nil {
			return err
		}
		settled = len(rows)
		return nil
	})
	return settled, err
}

// applyCredits posts the credits on destination shard d in one local tx,
// skipping any already applied. The unique (transfer_id, direction) on
// Entries backstops that check against a concurrent relay.
func (l *Ledger) applyCredits(ctx context.Context, d int, rows []store.OutboxRow) error {
	return l.runX(ctx, "relay", func(x *store.XTx) error {
		tx, err := x.On(d)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, len(rows))
		for k, r := range rows {
			ids[k] = r.TransferID
		}
		done, err := store.CreditedTransfers(ctx, tx, ids)
		if err != nil {
			return err
		}
		var legs []leg
		for _, r := range rows {
			if !done[r.TransferID] {
				legs = append(legs, leg{r.TransferID, r.DestID, "credit", r.Amount})
			}
		}
		if l.cfg.CASStats != nil {
			l.cfg.CASStats.Add("saga.relayed", int64(len(legs)))
			l.cfg.CASStats.Add("saga.already_applied", int64(len(rows)-len(legs)))
		}
		return postLegs(ctx, x, legs)
	})
}
