package ledger

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

func (l *Ledger) GetTransfer(ctx context.Context, id uuid.UUID) (store.Transfer, error) {
	t, err := store.GetTransfer(ctx, l.db, id)
	if errors.Is(err, store.ErrTransferNotFound) {
		return store.Transfer{}, &NotFoundError{Resource: "transfer", ID: id.String()}
	}
	return t, err
}

type Balance struct {
	Posted         int64
	Available      int64
	ActiveHolds    []store.Hold
	ReceivableOwed int64
}

// GetBalance reads posted, Available, active Holds and Receivable from one
// snapshot, so the numbers agree with each other.
func (l *Ledger) GetBalance(ctx context.Context, accountID uuid.UUID) (Balance, error) {
	var b Balance
	err := pgx.BeginTxFunc(ctx, l.db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			a, err := l.account(ctx, tx, accountID)
			if err != nil {
				return err
			}
			if b.ActiveHolds, err = store.ActiveHolds(ctx, tx, accountID); err != nil {
				return err
			}
			if b.ReceivableOwed, err = store.ReceivableOwed(ctx, tx, accountID); err != nil {
				return err
			}
			b.Posted, b.Available = a.Posted, a.Posted
			for _, h := range b.ActiveHolds {
				b.Available -= h.Amount
			}
			return nil
		})
	return b, err
}
