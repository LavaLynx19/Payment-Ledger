package checker

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payment-ledger/internal/store"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	if _, err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// Each corruption is applied inside a tx that's rolled back, so the shared
// database stays clean for other tests.
func TestChecksDetectCorruption(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	cases := []struct {
		check   string // name of the Check that must fire
		corrupt func(tx pgx.Tx, wallet uuid.UUID) error
	}{
		{"wallet posted and available non-negative", func(tx pgx.Tx, w uuid.UUID) error {
			return postPair(ctx, tx, w, -50) // overdrawn through balanced Entries
		}},
		{"wallet never negative in history", func(tx pgx.Tx, w uuid.UUID) error {
			// Overdrawn, then refilled: only the Entry history shows it.
			if err := postPair(ctx, tx, w, -50); err != nil {
				return err
			}
			return postPair(ctx, tx, w, 100)
		}},
		{"posted matches entries and latest balance_after", func(tx pgx.Tx, w uuid.UUID) error {
			_, err := tx.Exec(ctx, `UPDATE accounts SET posted = posted + 1 WHERE id = $1`, w)
			return err
		}},
		// Global checks (A§9.6), computed across shards in Go.
		{"each transfer balanced", func(tx pgx.Tx, w uuid.UUID) error {
			if err := postPair(ctx, tx, w, 50); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE entries SET amount = amount + 1 WHERE account_id = $1`, w)
			return err
		}},
		{"cross-shard references resolve", func(tx pgx.Tx, w uuid.UUID) error {
			_, err := tx.Exec(ctx, `INSERT INTO entries (id, transfer_id, account_id, direction, amount, balance_after, account_version, created_at)
				VALUES ($1, $2, $3, 'credit', 1, 1, 999, clock_timestamp())`, uuid.New(), uuid.New(), w)
			return err
		}},
		{"each idempotency key maps to an existing transfer", func(tx pgx.Tx, w uuid.UUID) error {
			_, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (key, request_hash, transfer_id) VALUES ($1, 'h', $2)`,
				"checker-"+uuid.NewString(), uuid.New())
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.check, func(t *testing.T) {
			w, err := store.CreateWallets(ctx, db, 1)
			if err != nil {
				t.Fatal(err)
			}
			rollback := errors.New("rollback")
			err = pgx.BeginTxFunc(ctx, db, pgx.TxOptions{}, func(tx pgx.Tx) error {
				if err := c.corrupt(tx, w[0]); err != nil {
					t.Fatal(err)
				}
				results, err := Run(ctx, []store.Querier{tx})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, r := range results {
					if r.Name == c.check {
						found = true
						if r.Violations == 0 {
							t.Errorf("check %q missed the corruption", r.Name)
						}
					}
				}
				if !found {
					t.Fatalf("no check named %q", c.check)
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
		})
	}
}

// postPair moves amount into wallet from a throwaway System account through a
// posted Transfer with balanced Entries, bypassing all ledger checks. A
// negative amount overdraws the Wallet.
func postPair(ctx context.Context, tx pgx.Tx, wallet uuid.UUID, amount int64) error {
	sys, tr, hold := uuid.New(), uuid.New(), uuid.New()
	// The wallet Entry's version must be unique per account, so bump first and
	// read the new version back in the entry insert below.
	dir, sysDir, abs := "credit", "debit", amount
	if amount < 0 {
		dir, sysDir, abs = "debit", "credit", -amount
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		// Debit-normal System account: debiting it by amount raises posted by amount.
		{`INSERT INTO accounts (id, kind, subtype, normal_balance, posted, version) VALUES ($1, 'system', 'funding', 'debit', $2, 1)`,
			[]any{sys, amount}},
		{`UPDATE accounts SET posted = posted + $2, version = version + 1 WHERE id = $1`, []any{wallet, amount}},
		{`INSERT INTO transfers (id, type, source_id, dest_id, amount, status) VALUES ($1, 'p2p', $2, $3, $4, 'posted')`,
			[]any{tr, sys, wallet, abs}},
		{`INSERT INTO holds (id, transfer_id, source_id, dest_id, amount, status) VALUES ($1, $2, $3, $4, $5, 'captured')`,
			[]any{hold, tr, sys, wallet, abs}},
		{`INSERT INTO entries (id, transfer_id, account_id, direction, amount, balance_after, account_version, created_at)
		  SELECT $1, $2, $3, $4, $5, posted, version, clock_timestamp() FROM accounts WHERE id = $3`,
			[]any{uuid.New(), tr, wallet, dir, abs}},
		{`INSERT INTO entries (id, transfer_id, account_id, direction, amount, balance_after, account_version, created_at)
		  VALUES ($1, $2, $3, $4, $5, $6, 1, clock_timestamp())`,
			[]any{uuid.New(), tr, sys, sysDir, abs, amount}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return err
		}
	}
	return nil
}
