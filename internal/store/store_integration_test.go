package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB connects to the Compose Postgres named by DATABASE_URL and applies
// migrations. Tests that need it are skipped when DATABASE_URL is unset.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	if _, err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func newWallet(t *testing.T, db *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ids, err := CreateWallets(context.Background(), db, 1)
	if err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

func TestCASAccountRejectsStaleVersion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	id := newWallet(t, db)

	err := WithTx(ctx, db, func(tx pgx.Tx) error {
		posted, version, err := CASAccount(ctx, tx, id, 0, 50)
		if err != nil {
			return err
		}
		if posted != 50 || version != 1 {
			t.Errorf("got posted=%d version=%d, want 50/1", posted, version)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = WithTx(ctx, db, func(tx pgx.Tx) error {
		_, _, err := CASAccount(ctx, tx, id, 0, 50) // version 0 is now stale
		return err
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale CAS err = %v, want ErrVersionConflict", err)
	}
}

// Concurrent read-then-CAS writers must neither lose updates nor double-apply them.
func TestRunCASConcurrentWritersLoseNothing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	id := newWallet(t, db)

	const writers, perWriter = 20, 10
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	for range writers {
		wg.Go(func() {
			for range perWriter {
				errs <- RunCAS(ctx, db, 1000, nil, "test", func(tx pgx.Tx) error {
					a, err := GetAccount(ctx, tx, id)
					if err != nil {
						return err
					}
					_, _, err = CASAccount(ctx, tx, id, a.Version, 1)
					return err
				})
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	var posted, version int64
	if err := db.QueryRow(ctx, `SELECT posted, version FROM accounts WHERE id = $1`, id).Scan(&posted, &version); err != nil {
		t.Fatal(err)
	}
	if want := int64(writers * perWriter); posted != want || version != want {
		t.Errorf("posted=%d version=%d, want %d/%d", posted, version, want, want)
	}
}

func TestAscendingMatchesPostgresOrder(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	ids := make([]uuid.UUID, 50)
	for i := range ids {
		ids[i] = uuid.New() // random v4, so order isn't just creation time
	}
	rows, err := db.Query(ctx, `SELECT id FROM unnest($1::uuid[]) AS t(id) ORDER BY id`, ids)
	if err != nil {
		t.Fatal(err)
	}
	pgOrder, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range Ascending(ids...) {
		if id != pgOrder[i] {
			t.Fatalf("position %d: Go %s, Postgres %s", i, id, pgOrder[i])
		}
	}
}
