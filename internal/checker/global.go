package checker

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/shard"
	"payment-ledger/internal/store"
)

// view is the compact projection of every shard the global checks need.
type view struct {
	n         int
	accounts  []map[uuid.UUID]bool // per shard
	transfers []map[uuid.UUID]transfer
	holds     map[uuid.UUID]hold   // by transfer id
	nets      map[uuid.UUID]netSum // by transfer id, summed across shards
	keys      map[string]uuid.UUID // idempotency key → transfer id
	inFlight  map[uuid.UUID]int64  // saga outbox: transfer id → credit not yet relayed
}

type transfer struct {
	status, typ string
	dest        uuid.UUID
	reverses    *uuid.UUID
	amount      int64
}

type hold struct {
	dest     uuid.UUID
	captured *int64
}

type netSum struct {
	net     int64 // Σ debits - Σ credits
	entries int
}

// exists reports whether id is in m on the shard id routes to.
func (v *view) exists(m []map[uuid.UUID]bool, id uuid.UUID) bool {
	return m[shard.Route(id, v.n)][id]
}

func (v *view) transferAt(id uuid.UUID) (transfer, bool) {
	t, ok := v.transfers[shard.Route(id, v.n)][id]
	return t, ok
}

func load(ctx context.Context, shards []store.Querier) (*view, error) {
	v := &view{n: len(shards), holds: map[uuid.UUID]hold{}, nets: map[uuid.UUID]netSum{},
		keys: map[string]uuid.UUID{}, inFlight: map[uuid.UUID]int64{}}
	for i, q := range shards {
		accounts := map[uuid.UUID]bool{}
		if err := each(ctx, q, `SELECT id FROM accounts`, func(r pgx.Rows) error {
			var id uuid.UUID
			err := r.Scan(&id)
			accounts[id] = true
			return err
		}); err != nil {
			return nil, fmt.Errorf("shard %d accounts: %w", i, err)
		}
		v.accounts = append(v.accounts, accounts)

		transfers := map[uuid.UUID]transfer{}
		if err := each(ctx, q, `SELECT id, status, type, dest_id, reverses_id, amount FROM transfers`, func(r pgx.Rows) error {
			var id uuid.UUID
			var t transfer
			err := r.Scan(&id, &t.status, &t.typ, &t.dest, &t.reverses, &t.amount)
			transfers[id] = t
			return err
		}); err != nil {
			return nil, fmt.Errorf("shard %d transfers: %w", i, err)
		}
		v.transfers = append(v.transfers, transfers)

		if err := each(ctx, q, `SELECT transfer_id, dest_id, captured_amount FROM holds`, func(r pgx.Rows) error {
			var id uuid.UUID
			var h hold
			err := r.Scan(&id, &h.dest, &h.captured)
			v.holds[id] = h
			return err
		}); err != nil {
			return nil, fmt.Errorf("shard %d holds: %w", i, err)
		}

		if err := each(ctx, q, `
			SELECT transfer_id, sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END), count(*)
			FROM entries GROUP BY transfer_id`, func(r pgx.Rows) error {
			var id uuid.UUID
			var net int64
			var n int
			err := r.Scan(&id, &net, &n)
			s := v.nets[id]
			v.nets[id] = netSum{net: s.net + net, entries: s.entries + n}
			return err
		}); err != nil {
			return nil, fmt.Errorf("shard %d entries: %w", i, err)
		}

		if err := each(ctx, q, `SELECT transfer_id, amount FROM outbox`, func(r pgx.Rows) error {
			var id uuid.UUID
			var amount int64
			err := r.Scan(&id, &amount)
			v.inFlight[id] = amount
			return err
		}); err != nil {
			return nil, fmt.Errorf("shard %d outbox: %w", i, err)
		}

		if err := each(ctx, q, `SELECT key, transfer_id FROM idempotency_keys`, func(r pgx.Rows) error {
			var k string
			var id uuid.UUID
			err := r.Scan(&k, &id)
			v.keys[k] = id
			return err
		}); err != nil {
			return nil, fmt.Errorf("shard %d keys: %w", i, err)
		}
	}
	return v, nil
}

func each(ctx context.Context, q store.Querier, sql string, scan func(pgx.Rows) error) error {
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// runGlobal runs the checks whose rows span shards (A§9.6).
func runGlobal(ctx context.Context, shards []store.Querier) ([]Result, error) {
	v, err := load(ctx, shards)
	if err != nil {
		return nil, err
	}
	return v.evaluate(), nil
}

// evaluate runs the global checks on a loaded view.
func (v *view) evaluate() []Result {
	ledger := Result{Check: Check{Invariant: 1, Name: "ledger balanced (Σ debits = Σ credits)"}}
	perTransfer := Result{Check: Check{Invariant: 1, Name: "each transfer balanced"}}
	posted := Result{Check: Check{Invariant: 5, Name: "posted transfers have entries; others have none"}}
	keys := Result{Check: Check{Invariant: 6, Name: "each idempotency key maps to an existing transfer"}}
	reversals := Result{Check: Check{Invariant: 7, Name: "reversals return exactly the posted amount, once"}}
	refs := Result{Check: Check{Invariant: 8, Name: "cross-shard references resolve"}}

	// A saga credit still in the outbox leaves its Transfer, and the ledger,
	// off by exactly that amount until the relay applies it (A§9.5).
	var total, inFlight int64
	for _, amount := range v.inFlight {
		inFlight += amount
	}
	for id, s := range v.nets {
		total += s.net
		if off := s.net - v.inFlight[id]; off != 0 {
			perTransfer.add(fmt.Sprintf("transfer %s off by %d (in flight %d)", id, off, v.inFlight[id]))
		}
		if _, ok := v.transferAt(id); !ok {
			refs.add(fmt.Sprintf("entries reference missing transfer %s", id))
		}
	}
	if total != inFlight {
		ledger.add(fmt.Sprintf("ledger off by %d (in flight %d)", total-inFlight, inFlight))
	}

	type revTotal struct{ sum, reversals, receivables int }
	revs := map[uuid.UUID]*revTotal{}
	for i, transfers := range v.transfers {
		for id, t := range transfers {
			if (t.status == "posted") != (v.nets[id].entries > 0) {
				posted.add(fmt.Sprintf("transfer %s status=%s entries=%d", id, t.status, v.nets[id].entries))
			}
			if !v.exists(v.accounts, t.dest) {
				refs.add(fmt.Sprintf("shard %d: transfer %s dest %s missing on its shard", i, id, t.dest))
			}
			if t.reverses != nil {
				if _, ok := v.transferAt(*t.reverses); !ok {
					refs.add(fmt.Sprintf("shard %d: transfer %s reverses missing %s", i, id, *t.reverses))
				}
				r := revs[*t.reverses]
				if r == nil {
					r = &revTotal{}
					revs[*t.reverses] = r
				}
				r.sum += int(t.amount)
				switch t.typ {
				case "reversal":
					r.reversals++
				case "receivable":
					r.receivables++
				}
			}
		}
	}
	for orig, r := range revs {
		h, ok := v.holds[orig]
		if !ok || h.captured == nil {
			reversals.add(fmt.Sprintf("transfer %s reversed but has no captured hold", orig))
			continue
		}
		if int64(r.sum) != *h.captured || r.reversals > 1 || r.receivables > 1 {
			reversals.add(fmt.Sprintf("transfer %s posted %d but reversals total %d across %d+%d transfers",
				orig, *h.captured, r.sum, r.reversals, r.receivables))
		}
	}
	for id, h := range v.holds {
		if !v.exists(v.accounts, h.dest) {
			refs.add(fmt.Sprintf("hold of transfer %s dest %s missing on its shard", id, h.dest))
		}
	}
	for k, id := range v.keys {
		if _, ok := v.transferAt(id); !ok {
			keys.add(fmt.Sprintf("key %s points at missing transfer %s", k, id))
		}
	}
	return []Result{ledger, perTransfer, posted, keys, reversals, refs}
}
