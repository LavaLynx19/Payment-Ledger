package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrVersionConflict means another tx changed the Account since it was read.
	ErrVersionConflict = errors.New("store: account version changed")
	// ErrRetriesExhausted means every attempt of a CAS tx hit a version conflict.
	ErrRetriesExhausted = errors.New("store: CAS retries exhausted")
	ErrAccountNotFound  = errors.New("store: account not found")
)

// Account is the row the CAS helpers read and write.
type Account struct {
	ID            uuid.UUID
	Kind          string
	NormalBalance string
	Posted        int64
	Version       int64
}

// WithTx runs fn in one READ COMMITTED transaction. It commits when fn
// returns nil and rolls back otherwise.
func WithTx(ctx context.Context, db *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

// Counter receives CAS outcome counts keyed "<op>.attempts", "<op>.conflicts"
// and "<op>.exhausted". *expvar.Map satisfies it. Nil disables counting.
type Counter interface {
	Add(key string, delta int64)
}

// RunCAS runs fn in a fresh tx and retries it from scratch, with a fresh read,
// whenever fn returns ErrVersionConflict. After `attempts` conflicts it returns
// ErrRetriesExhausted. op labels the counts in stats.
func RunCAS(ctx context.Context, db *pgxpool.Pool, attempts int, stats Counter, op string, fn func(pgx.Tx) error) error {
	return retryOnConflict(ctx, attempts, stats, op, func() error { return WithTx(ctx, db, fn) })
}

func retryOnConflict(ctx context.Context, attempts int, stats Counter, op string, try func() error) error {
	for range attempts {
		count(stats, op+".attempts")
		err := try()
		if !errors.Is(err, ErrVersionConflict) {
			return err
		}
		count(stats, op+".conflicts")
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	count(stats, op+".exhausted")
	return ErrRetriesExhausted
}

func count(stats Counter, key string) {
	if stats != nil {
		stats.Add(key, 1)
	}
}

// GetAccount reads the Account row that a later CASAccount call will guard.
func GetAccount(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Account, error) {
	a := Account{ID: id}
	err := tx.QueryRow(ctx,
		`SELECT kind, normal_balance, posted, version FROM accounts WHERE id = $1`, id,
	).Scan(&a.Kind, &a.NormalBalance, &a.Posted, &a.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account: %w", err)
	}
	return a, nil
}

// CASAccount adds delta to posted and bumps version, but only if the version
// is still `expected`. A delta of 0 is a pure version bump (e.g. placing a
// Hold). It returns the new posted balance and version, or ErrVersionConflict.
func CASAccount(ctx context.Context, tx pgx.Tx, id uuid.UUID, expected, delta int64) (posted, version int64, err error) {
	err = tx.QueryRow(ctx,
		`UPDATE accounts SET posted = posted + $3, version = version + 1
		 WHERE id = $1 AND version = $2
		 RETURNING posted, version`, id, expected, delta,
	).Scan(&posted, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrVersionConflict
	}
	if err != nil {
		return 0, 0, fmt.Errorf("cas account: %w", err)
	}
	return posted, version, nil
}

// ApplyNet posts k legs to one Account in a single row update: debits and
// credits are the leg totals per direction, the sign comes from the
// Account's normal balance, and the version moves by k. It returns the new
// posted balance and version plus the normal balance, so callers can rebuild
// each leg's running balance. There's no version check: concurrent posters
// queue on the row lock instead of conflicting. Use it only where no funds
// check depends on the read (capture: funds were reserved at Accept).
func ApplyNet(ctx context.Context, tx pgx.Tx, id uuid.UUID, debits, credits int64, k int) (posted, version int64, normal string, err error) {
	err = tx.QueryRow(ctx,
		`UPDATE accounts
		 SET posted = posted + CASE WHEN normal_balance = 'debit' THEN $2::bigint - $3::bigint
		                            ELSE $3::bigint - $2::bigint END,
		     version = version + $4
		 WHERE id = $1
		 RETURNING posted, version, normal_balance`, id, debits, credits, k,
	).Scan(&posted, &version, &normal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, "", ErrAccountNotFound
	}
	if err != nil {
		return 0, 0, "", fmt.Errorf("apply net posting: %w", err)
	}
	return posted, version, normal, nil
}

// Ascending returns ids in the order a multi-Account tx must update them, so
// concurrent txs lock rows in the same order and can't deadlock. The byte
// order matches Postgres's uuid ordering.
func Ascending(ids ...uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out
}
