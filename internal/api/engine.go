package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"payment-ledger/internal/ledger"
	"payment-ledger/internal/store"
)

// Engine is the ledger behind the API: the sharded Postgres ledger
// (internal/ledger) or the TigerBeetle engine (internal/tbengine, A§9.7).
// Both return the ledger package's domain errors, which toConnect maps to
// A§7. An operation an engine doesn't support returns errors.ErrUnsupported.
type Engine interface {
	CreateTransfer(ctx context.Context, r ledger.AcceptRequest) (store.Transfer, error)
	TopUp(ctx context.Context, key string, hash []byte, walletID uuid.UUID, amount int64) (store.Transfer, error)
	Withdraw(ctx context.Context, key string, hash []byte, walletID uuid.UUID, amount int64) (store.Transfer, error)
	Repay(ctx context.Context, key string, hash []byte, walletID uuid.UUID, amount int64) (store.Transfer, error)
	PlaceHold(ctx context.Context, r ledger.AcceptRequest) (store.Transfer, store.Hold, error)
	CaptureHold(ctx context.Context, key string, hash []byte, holdID uuid.UUID, amount *int64) (store.Transfer, store.Hold, error)
	ReleaseHold(ctx context.Context, key string, hash []byte, holdID uuid.UUID) (store.Hold, error)
	ReverseTransfer(ctx context.Context, key string, hash []byte, transferID uuid.UUID) (reversal, receivable *store.Transfer, err error)

	GetTransfer(ctx context.Context, id uuid.UUID) (store.Transfer, error)
	GetBalance(ctx context.Context, accountID uuid.UUID) (ledger.Balance, error)
	GetBalanceAt(ctx context.Context, accountID uuid.UUID, at time.Time) (int64, error)
	ListEntries(ctx context.Context, accountID uuid.UUID, from, to *time.Time, afterVersion int64, limit int) ([]store.EntryRow, bool, error)
	ListReceivables(ctx context.Context, minAge time.Duration, afterDebtor uuid.UUID, limit int) ([]store.Receivable, bool, error)
}

var _ Engine = (*ledger.Ledger)(nil)
