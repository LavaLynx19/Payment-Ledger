package ledger

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

// AcceptRequest is a client request to move money, keyed for idempotency.
type AcceptRequest struct {
	Key      string
	Hash     []byte
	SourceID uuid.UUID
	DestID   uuid.UUID
	Amount   int64
	HoldTTL  time.Duration // 0 uses Config.HoldTTL
}

// CreateTransfer accepts a P2P Transfer between two Wallets.
func (l *Ledger) CreateTransfer(ctx context.Context, r AcceptRequest) (store.Transfer, error) {
	return l.accept(ctx, TypeP2P, r)
}

// TopUp accepts a Transfer from the funding System account into a Wallet.
func (l *Ledger) TopUp(ctx context.Context, key string, hash []byte, walletID uuid.UUID, amount int64) (store.Transfer, error) {
	return l.accept(ctx, TypeTopUp, AcceptRequest{Key: key, Hash: hash, SourceID: l.fundingID, DestID: walletID, Amount: amount})
}

// accept is A§5 Accept: claim the key, check funds, place the Hold, and return
// the Transfer as PENDING. A known key returns its original Transfer.
func (l *Ledger) accept(ctx context.Context, typ string, r AcceptRequest) (store.Transfer, error) {
	if r.Amount <= 0 {
		return store.Transfer{}, invalid("amount must be greater than 0.")
	}
	if r.SourceID == r.DestID {
		return store.Transfer{}, invalid("source_id and dest_id must be different accounts.")
	}
	ttl := r.HoldTTL
	if ttl == 0 {
		ttl = l.cfg.HoldTTL
	}
	if ttl < 0 {
		return store.Transfer{}, invalid("hold_ttl must be positive.")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.Transfer{}, err
	}

	var out store.Transfer
	err = store.RunCAS(ctx, l.db, l.cfg.CASAttempts, func(tx pgx.Tx) error {
		existing, claimed, err := store.ClaimIdempotencyKey(ctx, tx, r.Key, r.Hash, id)
		if err != nil {
			return err
		}
		if !claimed {
			out, err = store.GetTransfer(ctx, tx, existing)
			return err
		}

		src, err := l.account(ctx, tx, r.SourceID)
		if err != nil {
			return err
		}
		dst, err := l.account(ctx, tx, r.DestID)
		if err != nil {
			return err
		}
		if dst.Kind != "wallet" {
			return invalid("dest_id must be a wallet.")
		}
		if typ == TypeP2P && src.Kind != "wallet" {
			return invalid("source_id must be a wallet.")
		}

		if src.Kind == "wallet" {
			held, err := store.ActiveHoldTotal(ctx, tx, src.ID)
			if err != nil {
				return err
			}
			if available := src.Posted - held; available < r.Amount {
				return &InsufficientFundsError{Available: available, Amount: r.Amount}
			}
		}

		// Rung 1: intentionally naive. There's no CAS on the source, so two
		// concurrent Accepts can both pass the funds check above (Decision Log →
		// "Rung 1 ships an intentionally racy Accept"). P1.9 adds the CAS here.

		out, err = store.InsertPendingTransfer(ctx, tx, store.Transfer{
			ID: id, Type: typ, SourceID: src.ID, DestID: dst.ID, Amount: r.Amount,
		}, ttl)
		return err
	})
	return out, err
}
