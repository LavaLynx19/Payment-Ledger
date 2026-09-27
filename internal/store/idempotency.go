package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrIdempotencyMismatch means the key was already used with a different request.
var ErrIdempotencyMismatch = errors.New("store: idempotency key reused with a different request")

// ClaimIdempotencyKey binds key to transferID inside tx (A§5 Accept step 1).
//
//   - New key: returns (transferID, true). The caller then creates that Transfer
//     in the same tx; the FK is checked at commit.
//   - Same key and hash already committed: returns (existingID, false).
//   - Same key, different hash: returns ErrIdempotencyMismatch.
//
// If another uncommitted tx holds the key, the insert waits for it. On commit
// this call reports that tx's Transfer; on abort, the key is claimed here.
func ClaimIdempotencyKey(ctx context.Context, tx pgx.Tx, key string, hash []byte, transferID uuid.UUID) (uuid.UUID, bool, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO idempotency_keys (key, request_hash, transfer_id) VALUES ($1, $2, $3)
		 ON CONFLICT (key) DO NOTHING`, key, hash, transferID)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return transferID, true, nil
	}

	var storedHash []byte
	var existing uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT request_hash, transfer_id FROM idempotency_keys WHERE key = $1`, key,
	).Scan(&storedHash, &existing)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("read idempotency key: %w", err)
	}
	if !bytes.Equal(storedHash, hash) {
		return uuid.Nil, false, ErrIdempotencyMismatch
	}
	return existing, false, nil
}
