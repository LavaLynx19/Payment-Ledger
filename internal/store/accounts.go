// Package store owns all SQL: pgx pool, tx and CAS helpers, and queries.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payment-ledger/internal/shard"
)

// Open connects a pgx pool to dsn and verifies it with a ping.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// EnsureFundingAccount is EnsureFundingAccountOn for shard 0.
func EnsureFundingAccount(ctx context.Context, db *pgxpool.Pool) (uuid.UUID, error) {
	return EnsureFundingAccountOn(ctx, db, 0)
}

// EnsureFundingAccountOn returns the funding System account of the shard db
// belongs to (index s), creating it on first use. Each shard has its own
// funding account, so TopUps and Withdrawals stay single-shard (A§9.1).
func EnsureFundingAccountOn(ctx context.Context, db *pgxpool.Pool, s int) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRow(ctx, `SELECT id FROM accounts WHERE subtype = 'funding' LIMIT 1`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("find funding account: %w", err)
	}
	id, err = shard.NewID(s)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = db.Exec(ctx,
		`INSERT INTO accounts (id, kind, subtype, normal_balance) VALUES ($1, 'system', 'funding', 'debit')`, id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create funding account: %w", err)
	}
	return id, nil
}

// CreateWallets is CreateWalletsOn for shard 0.
func CreateWallets(ctx context.Context, db *pgxpool.Pool, n int) ([]uuid.UUID, error) {
	return CreateWalletsOn(ctx, db, 0, n)
}

// CreateWalletsOn inserts n empty Wallets into db, the pool of shard s, each
// minted with s's bits, and returns their IDs.
func CreateWalletsOn(ctx context.Context, db *pgxpool.Pool, s, n int) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, n)
	rows := make([][]any, n)
	for i := range ids {
		id, err := shard.NewID(s)
		if err != nil {
			return nil, err
		}
		ids[i] = id
		rows[i] = []any{id, "wallet", "credit"}
	}
	_, err := db.CopyFrom(ctx, pgx.Identifier{"accounts"},
		[]string{"id", "kind", "normal_balance"}, pgx.CopyFromRows(rows))
	if err != nil {
		return nil, fmt.Errorf("create wallets: %w", err)
	}
	return ids, nil
}
