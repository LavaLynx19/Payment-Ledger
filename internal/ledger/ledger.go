// Package ledger implements the A§5 write paths and the reads over them. All
// SQL lives in internal/store.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payment-ledger/internal/failpoint"
	"payment-ledger/internal/store"
)

// fail fires the named A§5 crash point if it's enabled.
func (l *Ledger) fail(name string) { l.cfg.Failpoints.Inject(name) }

// Transfer types, matching transfers.type.
const (
	TypeP2P        = "p2p"
	TypeTopUp      = "topup"
	TypeWithdrawal = "withdrawal"
	TypeReversal   = "reversal"
	TypeReceivable = "receivable"
	TypeRepayment  = "repayment"
)

type Config struct {
	HoldTTL      time.Duration  // default expiry for client-initiated Holds
	CASAttempts  int            // tx attempts before CONFLICT_RETRIES_EXHAUSTED
	KeyRetention time.Duration  // how long idempotency keys are kept (A§2.7: 24h)
	Failpoints   *failpoint.Set // Rung 2 crash points; nil disables all
}

type Ledger struct {
	db        *pgxpool.Pool
	cfg       Config
	fundingID uuid.UUID
}

func New(ctx context.Context, db *pgxpool.Pool, cfg Config) (*Ledger, error) {
	fundingID, err := store.FundingAccountID(ctx, db)
	if errors.Is(err, store.ErrAccountNotFound) {
		return nil, errors.New("funding account missing; run cmd/seed first")
	}
	if err != nil {
		return nil, err
	}
	return &Ledger{db: db, cfg: cfg, fundingID: fundingID}, nil
}

// Domain errors. internal/api maps each one to its A§7 reason.

type InsufficientFundsError struct{ Available, Amount int64 }

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("insufficient funds: available %d < amount %d", e.Available, e.Amount)
}

// ReceivableOpenError blocks a debit from a Wallet that still owes Owed.
type ReceivableOpenError struct{ Owed int64 }

func (e *ReceivableOpenError) Error() string { return fmt.Sprintf("receivable open: owes %d", e.Owed) }

type NotFoundError struct{ Resource, ID string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("%s %s not found", e.Resource, e.ID) }

// InvalidError carries a message that names the field and the rule it broke.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

var (
	ErrIdempotencyMismatch = store.ErrIdempotencyMismatch
	ErrRetriesExhausted    = store.ErrRetriesExhausted
)

func invalid(msg string) error { return &InvalidError{Msg: msg} }

func (l *Ledger) account(ctx context.Context, tx pgx.Tx, id uuid.UUID) (store.Account, error) {
	a, err := store.GetAccount(ctx, tx, id)
	if errors.Is(err, store.ErrAccountNotFound) {
		return store.Account{}, &NotFoundError{Resource: "account", ID: id.String()}
	}
	return a, err
}
