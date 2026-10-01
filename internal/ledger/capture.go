package ledger

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

// CaptureNext captures one active Hold in full and posts its Transfer. It
// reports false when no Hold is waiting.
func (l *Ledger) CaptureNext(ctx context.Context) (bool, error) {
	n, err := l.CaptureBatch(ctx, 1)
	return n > 0, err
}

// CaptureBatch captures up to max of the oldest active Holds in one tx (A§5
// Capture, batched). It takes whatever is capturable now without waiting, so
// at low load it's a batch of one. It returns how many Holds it claimed,
// including any that lost to expiry and failed.
func (l *Ledger) CaptureBatch(ctx context.Context, max int) (int, error) {
	var claimed int
	err := l.runCAS(ctx, "capture", func(tx pgx.Tx) error {
		holds, err := store.ClaimCapturableHolds(ctx, tx, max)
		claimed = len(holds)
		if err != nil || claimed == 0 {
			return err
		}
		l.fail("capture.after_claim")

		ids := make([]uuid.UUID, len(holds))
		for i, h := range holds {
			ids[i] = h.ID
		}
		won, err := store.MarkHoldsCaptured(ctx, tx, ids)
		if err != nil {
			return err
		}
		var posted, failed []uuid.UUID
		var legs []leg
		for _, h := range holds {
			if !won[h.ID] { // expiry committed first
				failed = append(failed, h.TransferID)
				continue
			}
			posted = append(posted, h.TransferID)
			legs = append(legs, transferLegs(h.TransferID, h.SourceID, h.DestID, h.Amount)...)
		}
		if err := postLegs(ctx, tx, legs); err != nil {
			return err
		}
		l.fail("capture.after_entries")
		if err := store.SetTransfersStatus(ctx, tx, failed, "failed"); err != nil {
			return err
		}
		return store.SetTransfersStatus(ctx, tx, posted, "posted")
	})
	if err == nil && claimed > 0 {
		l.fail("capture.after_commit")
	}
	return claimed, err
}

// leg is one side of a posting: a debit of the source or a credit of the
// destination.
type leg struct {
	transferID uuid.UUID
	accountID  uuid.UUID
	direction  string
	amount     int64
}

func transferLegs(transferID, sourceID, destID uuid.UUID, amount int64) []leg {
	return []leg{
		{transferID, sourceID, "debit", amount},
		{transferID, destID, "credit", amount},
	}
}

// post debits the source and credits the destination of one Transfer.
func post(ctx context.Context, tx pgx.Tx, transferID, sourceID, destID uuid.UUID, amount int64) error {
	return postLegs(ctx, tx, transferLegs(transferID, sourceID, destID, amount))
}

// postLegs applies legs with one netted row update per Account, in ascending
// id order so concurrent batches can't deadlock. Each Account's legs get
// consecutive versions and a running balance_after, rebuilt from the update's
// returned totals. There's no CAS: debits were reserved at Accept, so
// concurrent captures queue on a hot row instead of retrying (Decision Log →
// "Rung 3: version checks only where a funds check needs them").
func postLegs(ctx context.Context, tx pgx.Tx, legs []leg) error {
	if len(legs) == 0 {
		return nil
	}
	byAccount := map[uuid.UUID][]leg{}
	for _, lg := range legs {
		byAccount[lg.accountID] = append(byAccount[lg.accountID], lg)
	}
	accounts := make([]uuid.UUID, 0, len(byAccount))
	for id := range byAccount {
		accounts = append(accounts, id)
	}

	entries := make([]store.Entry, 0, len(legs))
	for _, id := range store.Ascending(accounts...) {
		ls := byAccount[id]
		var debits, credits int64
		for _, lg := range ls {
			if lg.direction == "debit" {
				debits += lg.amount
			} else {
				credits += lg.amount
			}
		}
		posted, version, normal, err := store.ApplyNet(ctx, tx, id, debits, credits, len(ls))
		if err != nil {
			return err
		}
		// Walk forward from the balance and version before this update.
		signed := func(lg leg) int64 {
			if lg.direction == normal {
				return lg.amount
			}
			return -lg.amount
		}
		balance := posted
		for _, lg := range ls {
			balance -= signed(lg)
		}
		v := version - int64(len(ls))
		for _, lg := range ls {
			balance += signed(lg)
			v++
			entries = append(entries, store.Entry{
				TransferID: lg.transferID, AccountID: id, Direction: lg.direction,
				Amount: lg.amount, BalanceAfter: balance, AccountVersion: v,
			})
		}
	}
	return store.InsertEntries(ctx, tx, entries)
}
