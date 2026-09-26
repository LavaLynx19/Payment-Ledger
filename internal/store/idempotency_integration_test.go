package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// insertTransfer creates a bare pending Transfer so a claimed key's deferred
// FK is satisfied at commit.
func insertTransfer(t *testing.T, ctx context.Context, tx pgx.Tx, src, dst uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	_, err := tx.Exec(ctx,
		`INSERT INTO transfers (id, type, source_id, dest_id, amount, status) VALUES ($1, 'p2p', $2, $3, 1, 'pending')`,
		id, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestClaimIdempotencyKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	src, dst := newWallet(t, db), newWallet(t, db)
	key := "idem-" + uuid.NewString()
	hash := []byte("hash-a")

	var first uuid.UUID
	err := WithTx(ctx, db, func(tx pgx.Tx) error {
		id := insertTransfer(t, ctx, tx, src, dst)
		got, claimed, err := ClaimIdempotencyKey(ctx, tx, key, hash, id)
		if err != nil || !claimed || got != id {
			t.Fatalf("new key: got=%s claimed=%v err=%v", got, claimed, err)
		}
		first = id
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("same hash returns original", func(t *testing.T) {
		_ = WithTx(ctx, db, func(tx pgx.Tx) error {
			got, claimed, err := ClaimIdempotencyKey(ctx, tx, key, hash, uuid.Must(uuid.NewV7()))
			if err != nil || claimed || got != first {
				t.Errorf("got=%s claimed=%v err=%v, want %s/false/nil", got, claimed, err, first)
			}
			return errors.New("rollback")
		})
	})

	t.Run("different hash is a mismatch", func(t *testing.T) {
		_ = WithTx(ctx, db, func(tx pgx.Tx) error {
			_, _, err := ClaimIdempotencyKey(ctx, tx, key, []byte("hash-b"), uuid.Must(uuid.NewV7()))
			if !errors.Is(err, ErrIdempotencyMismatch) {
				t.Errorf("err = %v, want ErrIdempotencyMismatch", err)
			}
			return errors.New("rollback")
		})
	})
}

type claimResult struct {
	id      uuid.UUID
	claimed bool
	err     error
}

// A retry racing an uncommitted original must wait, then see the original's
// outcome: the original's Transfer if it committed, the key if it aborted.
func TestClaimIdempotencyKeyInFlight(t *testing.T) {
	for _, commit := range []bool{true, false} {
		name := map[bool]string{true: "original commits", false: "original aborts"}[commit]
		t.Run(name, func(t *testing.T) {
			db := testDB(t)
			ctx := context.Background()
			src, dst := newWallet(t, db), newWallet(t, db)
			key := "idem-" + uuid.NewString()
			hash := []byte("hash-a")

			tx1, err := db.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			original := insertTransfer(t, ctx, tx1, src, dst)
			if _, claimed, err := ClaimIdempotencyKey(ctx, tx1, key, hash, original); err != nil || !claimed {
				t.Fatalf("tx1 claim: claimed=%v err=%v", claimed, err)
			}

			retryID := uuid.Must(uuid.NewV7())
			done := make(chan claimResult, 1)
			go func() {
				tx2, err := db.Begin(ctx)
				if err != nil {
					done <- claimResult{err: err}
					return
				}
				defer tx2.Rollback(ctx)
				id, claimed, err := ClaimIdempotencyKey(ctx, tx2, key, hash, retryID)
				done <- claimResult{id, claimed, err}
			}()

			select {
			case r := <-done:
				t.Fatalf("retry returned before the original finished: %+v", r)
			case <-time.After(200 * time.Millisecond):
			}

			if commit {
				err = tx1.Commit(ctx)
			} else {
				err = tx1.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}

			r := <-done
			if r.err != nil {
				t.Fatal(r.err)
			}
			wantID, wantClaimed := original, false
			if !commit {
				wantID, wantClaimed = retryID, true
			}
			if r.id != wantID || r.claimed != wantClaimed {
				t.Errorf("retry got %s/%v, want %s/%v", r.id, r.claimed, wantID, wantClaimed)
			}
		})
	}
}
