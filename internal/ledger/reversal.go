package ledger

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

// NotReversibleError carries a message that names why the Transfer can't be
// reversed.
type NotReversibleError struct{ Msg string }

func (e *NotReversibleError) Error() string { return e.Msg }

// ReverseTransfer sends a posted P2P Transfer's money back (A§5 Reversal). The
// recipient returns what it has available. The rest becomes a Receivable the
// recipient owes, and the payer is made whole either way. Either returned
// Transfer may be nil. Replaying the key returns the same Transfers.
func (l *Ledger) ReverseTransfer(ctx context.Context, key string, hash []byte, transferID uuid.UUID) (reversal, receivable *store.Transfer, err error) {
	keyID, err := uuid.NewV7()
	if err != nil {
		return nil, nil, err
	}
	var claimed bool
	err = store.RunCAS(ctx, l.db, l.cfg.CASAttempts, func(tx pgx.Tx) error {
		reversal, receivable = nil, nil
		orig, err := store.LockTransfer(ctx, tx, transferID)
		if errors.Is(err, store.ErrTransferNotFound) {
			return &NotFoundError{Resource: "transfer", ID: transferID.String()}
		}
		if err != nil {
			return err
		}
		_, claimed, err = store.ClaimIdempotencyKey(ctx, tx, key, hash, keyID)
		if err != nil {
			return err
		}
		if !claimed {
			reversal, receivable, err = store.ReversalsOf(ctx, tx, orig.ID)
			return err
		}
		if reversal, receivable, err = l.reverse(ctx, tx, orig, keyID); err != nil {
			return err
		}
		l.fail("reversal.before_commit")
		return nil
	})
	if err == nil && claimed {
		l.fail("reversal.after_commit")
	}
	return reversal, receivable, err
}

func (l *Ledger) reverse(ctx context.Context, tx pgx.Tx, orig store.Transfer, keyID uuid.UUID) (reversal, receivable *store.Transfer, err error) {
	switch {
	case orig.Type != TypeP2P:
		return nil, nil, &NotReversibleError{Msg: "Only P2P transfers can be reversed."}
	case orig.Status != "posted":
		return nil, nil, &NotReversibleError{Msg: "Only posted transfers can be reversed; this one is " + orig.Status + "."}
	}
	priorRev, priorRecv, err := store.ReversalsOf(ctx, tx, orig.ID)
	if err != nil {
		return nil, nil, err
	}
	if priorRev != nil || priorRecv != nil {
		return nil, nil, &NotReversibleError{Msg: "This transfer has already been reversed."}
	}

	hold, err := store.GetHoldByTransfer(ctx, tx, orig.ID)
	if err != nil {
		return nil, nil, err
	}
	x := *hold.CapturedAmount // posted amount, which is less than orig.Amount after a partial capture
	payer, recipient := orig.SourceID, orig.DestID

	debtor, err := l.account(ctx, tx, recipient)
	if err != nil {
		return nil, nil, err
	}
	held, err := store.ActiveHoldTotal(ctx, tx, recipient)
	if err != nil {
		return nil, nil, err
	}
	r := min(x, max(debtor.Posted-held, 0))

	recv, err := store.EnsureReceivableAccount(ctx, tx, recipient)
	if err != nil {
		return nil, nil, err
	}
	// Bump both debtor rows, even when r = 0, so a concurrent Hold on the
	// recipient conflicts and re-reads the now-open Receivable (Decision Log).
	versions := map[uuid.UUID]int64{debtor.ID: debtor.Version, recv.ID: recv.Version}
	for _, id := range store.Ascending(debtor.ID, recv.ID) {
		if _, _, err := store.CASAccount(ctx, tx, id, versions[id], 0); err != nil {
			return nil, nil, err
		}
	}

	// The claimed key points at the first Transfer created, so keyID is used
	// exactly once.
	nextID := func() uuid.UUID {
		id := keyID
		keyID = uuid.Must(uuid.NewV7())
		return id
	}
	reverses := orig.ID
	if r > 0 {
		t, err := store.InsertPendingTransfer(ctx, tx, store.Transfer{
			ID: nextID(), Type: TypeReversal, SourceID: recipient, DestID: payer, Amount: r, ReversesID: &reverses,
		}, 0, CaptureAuto)
		if err != nil {
			return nil, nil, err
		}
		reversal = &t
	}
	if short := x - r; short > 0 {
		t, err := store.InsertPendingTransfer(ctx, tx, store.Transfer{
			ID: nextID(), Type: TypeReceivable, SourceID: recv.ID, DestID: payer, Amount: short, ReversesID: &reverses,
		}, 0, CaptureAuto)
		if err != nil {
			return nil, nil, err
		}
		receivable = &t
	}
	return reversal, receivable, nil
}
