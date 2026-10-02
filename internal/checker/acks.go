package checker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/shard"
	"payment-ledger/internal/store"
)

// Ack is a Transfer the harness's client was told succeeded: "ACK <key> <id>".
type Ack struct {
	Key        string
	TransferID uuid.UUID
}

// ParseAcks reads "<key> <transfer id>" lines (run.sh extracts them from k6's
// "ACK" log lines).
func ParseAcks(r io.Reader) ([]Ack, error) {
	var acks []Ack
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		id, err := uuid.Parse(f[1])
		if err != nil {
			return nil, fmt.Errorf("ack %q: %w", sc.Text(), err)
		}
		acks = append(acks, Ack{Key: f[0], TransferID: id})
	}
	return acks, sc.Err()
}

// AckCheck names the harness check: every acknowledged Transfer exists, is
// posted, and its key still maps to it. None lost, none duplicated under
// another id (Rung 2, A§9.6).
var AckCheck = Check{Invariant: 0, Name: "acknowledged transfers: none lost, all posted, one per key"}

// VerifyAcks checks acks against the sharded Postgres ledger. Transfers are
// looked up on the shard their id routes to, keys on their key's shard.
func VerifyAcks(ctx context.Context, shards []store.Querier, acks []Ack) (Result, error) {
	r := Result{Check: AckCheck}
	n := len(shards)
	byShard := make([][]uuid.UUID, n)
	keysByShard := make([][]string, n)
	for _, a := range acks {
		byShard[shard.Route(a.TransferID, n)] = append(byShard[shard.Route(a.TransferID, n)], a.TransferID)
		keysByShard[shard.ForKey(a.Key, n)] = append(keysByShard[shard.ForKey(a.Key, n)], a.Key)
	}
	status := map[uuid.UUID]string{}
	keyTo := map[string]uuid.UUID{}
	for i, q := range shards {
		if err := each(ctx, q, `SELECT id, status FROM transfers WHERE id = ANY($1)`, func(row pgx.Rows) error {
			var id uuid.UUID
			var s string
			err := row.Scan(&id, &s)
			status[id] = s
			return err
		}, byShard[i]); err != nil {
			return r, fmt.Errorf("shard %d acks: %w", i, err)
		}
		if err := each(ctx, q, `SELECT key, transfer_id FROM idempotency_keys WHERE key = ANY($1)`, func(row pgx.Rows) error {
			var k string
			var id uuid.UUID
			err := row.Scan(&k, &id)
			keyTo[k] = id
			return err
		}, keysByShard[i]); err != nil {
			return r, fmt.Errorf("shard %d ack keys: %w", i, err)
		}
	}
	for _, a := range acks {
		JudgeAck(&r, a, status[a.TransferID], keyTo[a.Key])
	}
	return r, nil
}

// JudgeAck records one ack's violation, if any. status is "" for a missing
// Transfer, and keyTo is uuid.Nil when the key is gone (purged).
func JudgeAck(r *Result, a Ack, status string, keyTo uuid.UUID) {
	switch {
	case status == "":
		r.add(fmt.Sprintf("lost: transfer %s (key %s) acknowledged but missing", a.TransferID, a.Key))
	case status != "posted":
		r.add(fmt.Sprintf("not posted: transfer %s is %s", a.TransferID, status))
	case keyTo != uuid.Nil && keyTo != a.TransferID:
		r.add(fmt.Sprintf("key %s maps to %s, not the acknowledged %s", a.Key, keyTo, a.TransferID))
	}
}
