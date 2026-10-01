package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

// Hold capture modes, matching holds.capture_mode.
const (
	CaptureAuto   = "auto"   // the worker captures it
	CaptureManual = "manual" // PlaceHold: the caller captures or releases it
)

type HoldNotActiveError struct{ Status string }

func (e *HoldNotActiveError) Error() string { return "hold is already " + e.Status }

type HoldExpiredError struct{ ExpiresAt time.Time }

func (e *HoldExpiredError) Error() string { return fmt.Sprintf("hold expired at %s", e.ExpiresAt) }

// PlaceHold reserves funds on a source Wallet for a fixed destination. The
// worker never captures it: the caller Captures or Releases it, or it expires.
func (l *Ledger) PlaceHold(ctx context.Context, r AcceptRequest) (store.Transfer, store.Hold, error) {
	t, err := l.accept(ctx, TypeP2P, CaptureManual, r)
	if err != nil {
		return store.Transfer{}, store.Hold{}, err
	}
	h, err := store.GetHoldByTransfer(ctx, l.db, t.ID)
	return t, h, err
}

// CaptureHold posts amount (the whole Hold when nil) to the Hold's
// destination and releases any remainder (A§5 CaptureHold). Replaying the
// same key returns the current Transfer and Hold.
func (l *Ledger) CaptureHold(ctx context.Context, key string, hash []byte, holdID uuid.UUID, amount *int64) (store.Transfer, store.Hold, error) {
	var t store.Transfer
	var h store.Hold
	err := l.runCAS(ctx, "capture_hold", func(tx pgx.Tx) error {
		var err error
		if h, err = l.lockManualHold(ctx, tx, holdID); err != nil {
			return err
		}
		_, claimed, err := store.ClaimIdempotencyKey(ctx, tx, key, hash, h.TransferID)
		if err != nil {
			return err
		}
		if claimed {
			if err := l.capture(ctx, tx, h, amount); err != nil {
				return err
			}
			if h, err = store.GetHoldByTransfer(ctx, tx, h.TransferID); err != nil {
				return err
			}
		}
		t, err = store.GetTransfer(ctx, tx, h.TransferID)
		return err
	})
	return t, h, err
}

func (l *Ledger) capture(ctx context.Context, tx pgx.Tx, h store.Hold, amount *int64) error {
	if h.Status != "active" {
		return &HoldNotActiveError{Status: h.Status}
	}
	amt := h.Amount
	if amount != nil {
		amt = *amount
	}
	if amt <= 0 || amt > h.Amount {
		return invalid(fmt.Sprintf("amount must be between 1 and the hold amount %d.", h.Amount))
	}
	captured, err := store.MarkHoldCaptured(ctx, tx, h.ID, amt)
	if err != nil {
		return err
	}
	if !captured {
		return expiredError(h)
	}
	if err := post(ctx, tx, h.TransferID, h.SourceID, h.DestID, amt); err != nil {
		return err
	}
	return store.SetTransferStatus(ctx, tx, h.TransferID, "posted")
}

// ReleaseHold ends a manual Hold without moving money. Its Transfer fails and
// the reserved amount returns to Available. Replaying the same key returns
// the current Hold.
func (l *Ledger) ReleaseHold(ctx context.Context, key string, hash []byte, holdID uuid.UUID) (store.Hold, error) {
	var h store.Hold
	err := l.runCAS(ctx, "release", func(tx pgx.Tx) error {
		var err error
		if h, err = l.lockManualHold(ctx, tx, holdID); err != nil {
			return err
		}
		_, claimed, err := store.ClaimIdempotencyKey(ctx, tx, key, hash, h.TransferID)
		if err != nil || !claimed {
			return err
		}
		if h.Status != "active" {
			return &HoldNotActiveError{Status: h.Status}
		}
		released, err := store.MarkHoldReleased(ctx, tx, h.ID)
		if err != nil {
			return err
		}
		if !released {
			return expiredError(h)
		}
		src, err := l.account(ctx, tx, h.SourceID)
		if err != nil {
			return err
		}
		if _, _, err := store.CASAccount(ctx, tx, src.ID, src.Version, 0); err != nil {
			return err
		}
		if err := store.SetTransferStatus(ctx, tx, h.TransferID, "failed"); err != nil {
			return err
		}
		h.Status = "released"
		l.fail("release.before_commit")
		return nil
	})
	return h, err
}

// lockManualHold locks the Hold for this tx and rejects Holds the worker owns.
func (l *Ledger) lockManualHold(ctx context.Context, tx pgx.Tx, id uuid.UUID) (store.Hold, error) {
	h, err := store.LockHold(ctx, tx, id)
	if errors.Is(err, store.ErrHoldNotFound) {
		return store.Hold{}, &NotFoundError{Resource: "hold", ID: id.String()}
	}
	if err != nil {
		return store.Hold{}, err
	}
	if h.CaptureMode != CaptureManual {
		return store.Hold{}, invalid("hold_id belongs to a transfer that is captured automatically.")
	}
	return h, nil
}

// expiredError reports an active Hold that failed its expiry check. Holds
// with no expiry never fail it, so a nil ExpiresAt is a bug.
func expiredError(h store.Hold) error {
	if h.ExpiresAt == nil {
		return fmt.Errorf("hold %s has no expiry but failed the active check", h.ID)
	}
	return &HoldExpiredError{ExpiresAt: *h.ExpiresAt}
}
