package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/shard"
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
	return l.accept(ctx, TypeP2P, CaptureAuto, r)
}

// TopUp accepts a Transfer from the funding System account into a Wallet.
func (l *Ledger) TopUp(ctx context.Context, key string, hash []byte, walletID uuid.UUID, amount int64) (store.Transfer, error) {
	return l.accept(ctx, TypeTopUp, CaptureAuto,
		AcceptRequest{Key: key, Hash: hash, SourceID: l.fundingID, DestID: walletID, Amount: amount})
}

// Withdraw accepts a Transfer from a Wallet into the funding System account.
func (l *Ledger) Withdraw(ctx context.Context, key string, hash []byte, walletID uuid.UUID, amount int64) (store.Transfer, error) {
	return l.accept(ctx, TypeWithdrawal, CaptureAuto,
		AcceptRequest{Key: key, Hash: hash, SourceID: walletID, DestID: l.fundingID, Amount: amount})
}

// Repay accepts a Repayment from a Wallet into its own receivable System
// account. It's the one debit an open Receivable doesn't block.
func (l *Ledger) Repay(ctx context.Context, key string, hash []byte, walletID uuid.UUID, amount int64) (store.Transfer, error) {
	recvID, err := store.ReceivableAccountID(ctx, l.db, walletID)
	if errors.Is(err, store.ErrAccountNotFound) {
		return store.Transfer{}, invalid("wallet_id has no receivable to repay.")
	}
	if err != nil {
		return store.Transfer{}, err
	}
	return l.accept(ctx, TypeRepayment, CaptureAuto,
		AcceptRequest{Key: key, Hash: hash, SourceID: walletID, DestID: recvID, Amount: amount})
}

// endpoints is which Account kind each Transfer type may use as source and
// destination. The ledger fills in System-account ids itself, so a mismatch
// always points at the caller's id field.
var endpoints = map[string]struct{ source, dest, field string }{
	TypeP2P:        {"wallet", "wallet", ""},
	TypeTopUp:      {"system", "wallet", "wallet_id"},
	TypeWithdrawal: {"wallet", "system", "wallet_id"},
	TypeRepayment:  {"wallet", "system", "wallet_id"},
}

// checkReceivable enforces the Receivable rules on a Wallet source: an open
// Receivable blocks every debit except a Repayment, and a Repayment can't
// exceed what's still owed once pending Repayments land. The caller has
// already read src, and its CAS on src makes this race-free against a
// Reversal, which bumps the debtor Wallet's version (Decision Log).
func checkReceivable(ctx context.Context, tx pgx.Tx, typ string, src, dst store.Account, amount int64) error {
	owed, err := store.ReceivableOwed(ctx, tx, src.ID)
	if err != nil {
		return err
	}
	if typ != TypeRepayment {
		if owed > 0 {
			return &ReceivableOpenError{Owed: owed}
		}
		return nil
	}
	pending, err := store.PendingInto(ctx, tx, dst.ID)
	if err != nil {
		return err
	}
	if remaining := owed - pending; amount > remaining {
		return invalid(fmt.Sprintf("amount must be at most %d, the amount still owed.", max(remaining, 0)))
	}
	return nil
}

func checkEndpoints(typ string, src, dst store.Account) error {
	e := endpoints[typ]
	switch {
	case src.Kind != e.source && e.field != "":
		return invalid(e.field + " must be a wallet.")
	case src.Kind != e.source:
		return invalid("source_id must be a " + e.source + ".")
	case dst.Kind != e.dest && e.field != "":
		return invalid(e.field + " must be a wallet.")
	case dst.Kind != e.dest:
		return invalid("dest_id must be a " + e.dest + ".")
	}
	return nil
}

// accept is A§5 Accept: claim the key, check funds, place the Hold, and return
// the Transfer as PENDING. A known key returns its original Transfer.
func (l *Ledger) accept(ctx context.Context, typ, captureMode string, r AcceptRequest) (store.Transfer, error) {
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
	id, err := shard.Like(r.SourceID) // the Transfer lives on its source's shard
	if err != nil {
		return store.Transfer{}, err
	}

	var out store.Transfer
	var claimed bool
	err = l.runCAS(ctx, "accept", func(tx pgx.Tx) error {
		var existing uuid.UUID
		var err error
		existing, claimed, err = store.ClaimIdempotencyKey(ctx, tx, r.Key, r.Hash, id)
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
		if err := checkEndpoints(typ, src, dst); err != nil {
			return err
		}

		if src.Kind == "wallet" {
			if err := checkReceivable(ctx, tx, typ, src, dst, r.Amount); err != nil {
				return err
			}
			held, err := store.ActiveHoldTotal(ctx, tx, src.ID)
			if err != nil {
				return err
			}
			if available := src.Posted - held; available < r.Amount {
				return &InsufficientFundsError{Available: available, Amount: r.Amount}
			}
		}

		// Bump a Wallet source's version even though posted doesn't change: a
		// concurrent Accept that read the same funds now conflicts, retries,
		// and sees this Hold (Decision Log → "Placing a Hold bumps the Account
		// version"). System sources have no funds check to protect, so they
		// skip the bump, which keeps the hot funding row out of every TopUp's
		// CAS (Decision Log → "Rung 3: version checks only where a funds check
		// needs them").
		if src.Kind == "wallet" {
			if _, _, err := store.CASAccount(ctx, tx, src.ID, src.Version, 0); err != nil {
				return err
			}
		}

		out, err = store.InsertPendingTransfer(ctx, tx, store.Transfer{
			ID: id, Type: typ, SourceID: src.ID, DestID: dst.ID, Amount: r.Amount,
		}, ttl, captureMode)
		if err == nil {
			l.fail("accept.before_commit")
		}
		return err
	})
	if err == nil && claimed {
		l.fail("accept.after_commit")
	}
	return out, err
}
