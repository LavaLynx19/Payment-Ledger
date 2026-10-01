// Package ledger implements the A§5 write paths and the reads over them. All
// SQL lives in internal/store.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
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
	CASStats     store.Counter  // per-op CAS attempts/conflicts/exhausted; nil disables
	// PrepareTimeout is how long a 2PC may sit prepared with no decision
	// before the resolver aborts it (A§9.4).
	PrepareTimeout time.Duration
}

// Resolve finishes in-doubt 2PC writes on every shard (A§9.4).
func (l *Ledger) Resolve(ctx context.Context) (committed, rolledBack int, err error) {
	return l.shards.Resolve(ctx, l.cfg.PrepareTimeout, l.cfg.CASStats)
}

// runX runs fn as one cross-shard write (A§9.2) with retries, counting CAS
// outcomes under op. A write that touches one shard commits locally.
func (l *Ledger) runX(ctx context.Context, op string, fn func(*store.XTx) error) error {
	return l.shards.RunX(ctx, l.cfg.CASAttempts, l.cfg.CASStats, op, l.fail, fn)
}

type Ledger struct {
	shards     *store.Shards
	db         *pgxpool.Pool // shard 0, for single-shard helpers and tests
	cfg        Config
	fundingIDs []uuid.UUID // per shard: TopUp/Withdraw use the Wallet's own shard's (A§9.1)
}

func New(ctx context.Context, shards *store.Shards, cfg Config) (*Ledger, error) {
	l := &Ledger{shards: shards, db: shards.Pool(0), cfg: cfg}
	for i := range shards.N() {
		id, err := store.FundingAccountID(ctx, shards.Pool(i))
		if errors.Is(err, store.ErrAccountNotFound) {
			return nil, fmt.Errorf("shard %d: funding account missing; run cmd/seed first", i)
		}
		if err != nil {
			return nil, err
		}
		l.fundingIDs = append(l.fundingIDs, id)
	}
	return l, nil
}

// fundingFor is the funding account on wallet's shard.
func (l *Ledger) fundingFor(wallet uuid.UUID) uuid.UUID {
	return l.fundingIDs[l.shardOf(wallet)]
}

// keyShard is the shard an Idempotency-Key lives on: FNV-1a(key) mod N
// (A§9.2). With one shard it's always 0.
func (l *Ledger) keyShard(key string) int {
	h := fnv.New64a()
	h.Write([]byte(key))
	return int(h.Sum64() % uint64(l.shards.N()))
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

func (l *Ledger) account(ctx context.Context, q store.Querier, id uuid.UUID) (store.Account, error) {
	a, err := store.GetAccount(ctx, q, id)
	if errors.Is(err, store.ErrAccountNotFound) {
		return store.Account{}, &NotFoundError{Resource: "account", ID: id.String()}
	}
	return a, err
}
