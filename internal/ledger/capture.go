package ledger

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

// CaptureNext captures one active Hold in full and posts its Transfer (A§5
// Capture). It reports false when no Hold is waiting.
func (l *Ledger) CaptureNext(ctx context.Context) (bool, error) {
	var found bool
	err := store.RunCAS(ctx, l.db, l.cfg.CASAttempts, func(tx pgx.Tx) error {
		h, ok, err := store.ClaimCapturableHold(ctx, tx)
		found = ok
		if err != nil || !ok {
			return err
		}
		captured, err := store.MarkHoldCaptured(ctx, tx, h.ID, h.Amount)
		if err != nil {
			return err
		}
		if !captured { // expiry committed first
			return store.SetTransferStatus(ctx, tx, h.TransferID, "failed")
		}
		if err := post(ctx, tx, h.TransferID, h.SourceID, h.DestID, h.Amount); err != nil {
			return err
		}
		return store.SetTransferStatus(ctx, tx, h.TransferID, "posted")
	})
	return found, err
}

// post debits the source and credits the destination. Accounts are updated in
// ascending id order, and each Entry records the balance and version its CAS
// produced.
func post(ctx context.Context, tx pgx.Tx, transferID, sourceID, destID uuid.UUID, amount int64) error {
	direction := map[uuid.UUID]string{sourceID: "debit", destID: "credit"}
	for _, id := range store.Ascending(sourceID, destID) {
		a, err := store.GetAccount(ctx, tx, id)
		if err != nil {
			return err
		}
		dir := direction[id]
		delta := amount
		if dir != a.NormalBalance {
			delta = -amount
		}
		posted, version, err := store.CASAccount(ctx, tx, id, a.Version, delta)
		if err != nil {
			return err
		}
		if err := store.InsertEntry(ctx, tx, store.Entry{
			TransferID: transferID, AccountID: id, Direction: dir,
			Amount: amount, BalanceAfter: posted, AccountVersion: version,
		}); err != nil {
			return err
		}
	}
	return nil
}
