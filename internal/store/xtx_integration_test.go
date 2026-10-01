package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"payment-ledger/internal/shard"
)

// noLeftovers asserts no shard holds a prepared tx or an undeleted decision.
func noLeftovers(t *testing.T, s *Shards) {
	t.Helper()
	for i := range s.N() {
		var prepared, decisions int
		if err := s.Pool(i).QueryRow(context.Background(),
			`SELECT (SELECT count(*) FROM pg_prepared_xacts), (SELECT count(*) FROM decisions)`,
		).Scan(&prepared, &decisions); err != nil {
			t.Fatal(err)
		}
		if prepared != 0 || decisions != 0 {
			t.Errorf("shard %d: %d prepared txs and %d decisions left", i, prepared, decisions)
		}
	}
}

// bump writes an account row on its own shard through x.
func bump(ctx context.Context, x *XTx, id uuid.UUID) error {
	tx, err := x.For(id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE accounts SET version = version + 1 WHERE id = $1`, id)
	return err
}

func versionOf(t *testing.T, s *Shards, id uuid.UUID) int64 {
	t.Helper()
	var v int64
	if err := s.For(id).QueryRow(context.Background(), `SELECT version FROM accounts WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestXTxTwoPhaseCommit(t *testing.T) {
	s, _ := twoShards(t)
	ctx := context.Background()
	a, err := CreateWalletsOn(ctx, s.Pool(0), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CreateWalletsOn(ctx, s.Pool(1), 1, 1)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("commit lands on both shards", func(t *testing.T) {
		err := s.RunX(ctx, 1, nil, "test", func(x *XTx) error {
			x.SetHome(a[0])
			if err := bump(ctx, x, a[0]); err != nil {
				return err
			}
			return bump(ctx, x, b[0])
		})
		if err != nil {
			t.Fatal(err)
		}
		if versionOf(t, s, a[0]) != 1 || versionOf(t, s, b[0]) != 1 {
			t.Error("2PC commit didn't land on both shards")
		}
		noLeftovers(t, s)
	})

	t.Run("error after touching both rolls back both", func(t *testing.T) {
		boom := errors.New("boom")
		err := s.RunX(ctx, 1, nil, "test", func(x *XTx) error {
			x.SetHome(a[0])
			if err := bump(ctx, x, a[0]); err != nil {
				return err
			}
			if err := bump(ctx, x, b[0]); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		if versionOf(t, s, a[0]) != 1 || versionOf(t, s, b[0]) != 1 {
			t.Error("aborted write changed a shard")
		}
		noLeftovers(t, s)
	})

	t.Run("panic leaks no locks", func(t *testing.T) {
		func() {
			defer func() { _ = recover() }()
			_ = s.RunX(ctx, 1, nil, "test", func(x *XTx) error {
				_ = bump(ctx, x, a[0])
				_ = bump(ctx, x, b[0])
				panic("crash")
			})
		}()
		done := make(chan error, 1)
		go func() {
			done <- s.RunX(ctx, 1, nil, "test", func(x *XTx) error {
				x.SetHome(a[0])
				if err := bump(ctx, x, a[0]); err != nil {
					return err
				}
				return bump(ctx, x, b[0])
			})
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("rows still locked after a panicking write")
		}
		noLeftovers(t, s)
	})

	t.Run("gid routes back to its home shard", func(t *testing.T) {
		home, _ := shard.NewID(1)
		got, ok := HomeOf(GID(home))
		if !ok || got != home || shard.Route(got, s.N()) != 1 {
			t.Errorf("HomeOf(GID) = %s, %v", got, ok)
		}
	})
}
