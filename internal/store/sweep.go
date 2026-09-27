package store

import (
	"context"
	"fmt"
	"time"
)

// ExpireHolds finalizes up to batch Holds past their expiry: the Hold becomes
// expired and its pending Transfer fails. Reads already ignore these Holds
// (lazy expiry), so this only settles the records. SKIP LOCKED leaves a Hold
// that a capture is working on to that capture, and whichever commits first wins.
func ExpireHolds(ctx context.Context, q Querier, batch int) (int64, error) {
	tag, err := q.Exec(ctx,
		`WITH due AS (
		     SELECT id FROM holds
		     WHERE status = 'active' AND expires_at <= clock_timestamp()
		     ORDER BY expires_at LIMIT $1
		     FOR UPDATE SKIP LOCKED
		 ), expired AS (
		     UPDATE holds h SET status = 'expired' FROM due WHERE h.id = due.id
		     RETURNING h.transfer_id
		 )
		 UPDATE transfers t SET status = 'failed'
		 FROM expired WHERE t.id = expired.transfer_id AND t.status = 'pending'`, batch)
	if err != nil {
		return 0, fmt.Errorf("expire holds: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PurgeIdempotencyKeys deletes up to batch keys older than retention. A
// retry after that is treated as a new request (A§2.7).
func PurgeIdempotencyKeys(ctx context.Context, q Querier, retention time.Duration, batch int) (int64, error) {
	tag, err := q.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE key IN (
		     SELECT key FROM idempotency_keys
		     WHERE created_at < clock_timestamp() - $1::bigint * interval '1 microsecond'
		     LIMIT $2
		 )`, retention.Microseconds(), batch)
	if err != nil {
		return 0, fmt.Errorf("purge idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}
