package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OutboxRow is a credit leg waiting to be relayed to its destination's shard
// (A§9.5 saga variant).
type OutboxRow struct {
	TransferID uuid.UUID
	DestID     uuid.UUID
	Amount     int64
}

// InsertOutbox records credits bound for other shards, in the capture's tx on
// the source shard.
func InsertOutbox(ctx context.Context, tx pgx.Tx, rows []OutboxRow) error {
	for _, r := range rows {
		if _, err := tx.Exec(ctx,
			`INSERT INTO outbox (transfer_id, dest_id, amount) VALUES ($1, $2, $3)`,
			r.TransferID, r.DestID, r.Amount); err != nil {
			return fmt.Errorf("insert outbox: %w", err)
		}
	}
	return nil
}

// ClaimOutbox locks up to limit of the oldest outbox rows that no other relay
// holds (FOR UPDATE SKIP LOCKED).
func ClaimOutbox(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxRow, error) {
	rows, err := tx.Query(ctx,
		`SELECT transfer_id, dest_id, amount FROM outbox ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OutboxRow, error) {
		var o OutboxRow
		err := r.Scan(&o.TransferID, &o.DestID, &o.Amount)
		return o, err
	})
}

// DeleteOutbox removes relayed rows.
func DeleteOutbox(ctx context.Context, tx pgx.Tx, transferIDs []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM outbox WHERE transfer_id = ANY($1)`, transferIDs); err != nil {
		return fmt.Errorf("delete outbox: %w", err)
	}
	return nil
}

// CreditedTransfers returns which of transferIDs already have a credit Entry
// on this shard: credits a crashed relay applied before deleting its rows.
func CreditedTransfers(ctx context.Context, q Querier, transferIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := q.Query(ctx,
		`SELECT transfer_id FROM entries WHERE direction = 'credit' AND transfer_id = ANY($1)`, transferIDs)
	if err != nil {
		return nil, fmt.Errorf("credited transfers: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	done := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		done[id] = true
	}
	return done, nil
}
