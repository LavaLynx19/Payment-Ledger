// Package checker verifies the A§4 ledger invariants across every shard
// (A§9.6). Local checks are SQL run on each shard; global checks span shards
// and are computed in Go from a compact projection of every shard. Each
// check yields one human-readable detail per violation.
package checker

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

type Check struct {
	Invariant int
	Name      string
	SQL       string // local checks only: SELECT detail text, one row per violation
}

// Local are the invariants each shard can verify on its own.
var Local = []Check{
	{2, "posted matches entries and latest balance_after", `
		WITH sums AS (
			SELECT e.account_id, sum(CASE WHEN e.direction = a.normal_balance THEN e.amount ELSE -e.amount END) AS s
			FROM entries e JOIN accounts a ON a.id = e.account_id GROUP BY e.account_id
		), latest AS (
			SELECT DISTINCT ON (account_id) account_id, balance_after
			FROM entries ORDER BY account_id, account_version DESC
		)
		SELECT 'account ' || a.id || ' posted=' || a.posted || ' entries=' || coalesce(s.s, 0)
		       || ' latest_balance_after=' || coalesce(l.balance_after::text, 'none')
		FROM accounts a
		LEFT JOIN sums s ON s.account_id = a.id
		LEFT JOIN latest l ON l.account_id = a.id
		WHERE a.posted <> coalesce(s.s, 0) OR l.balance_after <> a.posted`},
	{3, "wallet posted and available non-negative", `
		SELECT 'wallet ' || a.id || ' posted=' || a.posted || ' available=' || (a.posted - coalesce(h.held, 0))
		FROM accounts a
		LEFT JOIN (
			SELECT source_id, sum(amount) AS held FROM holds
			WHERE status = 'active' AND (expires_at IS NULL OR expires_at > clock_timestamp())
			GROUP BY source_id
		) h ON h.source_id = a.id
		WHERE a.kind = 'wallet' AND (a.posted < 0 OR a.posted - coalesce(h.held, 0) < 0)`},
	// Later credits can refill an overspent Wallet, so check every balance it passed through.
	{3, "wallet never negative in history", `
		SELECT 'wallet ' || e.account_id || ' went to ' || e.balance_after || ' at version ' || e.account_version
		FROM entries e JOIN accounts a ON a.id = e.account_id
		WHERE a.kind = 'wallet' AND e.balance_after < 0
		ORDER BY e.balance_after`},
	{4, "entry versions strictly increase per account", `
		SELECT 'account ' || account_id || ' entry ' || id || ' version ' || account_version || ' after ' || prev
		FROM (
			SELECT account_id, id, account_version,
			       lag(account_version) OVER (PARTITION BY account_id ORDER BY created_at, account_version) AS prev
			FROM entries
		) x WHERE prev >= account_version
		UNION ALL
		SELECT 'account ' || e.account_id || ' entry version ' || e.account_version || ' > account version ' || a.version
		FROM entries e JOIN accounts a ON a.id = e.account_id WHERE e.account_version > a.version`},
	// A Hold shares its Transfer's shard; so does the debit Entry (on the source).
	{5, "posted transfers have a captured hold; others don't", `
		SELECT 'transfer ' || t.id || ' status=' || t.status || ' hold=' || coalesce(h.status, 'none')
		FROM transfers t LEFT JOIN holds h ON h.transfer_id = t.id
		WHERE (t.status = 'posted') <> (h.status IS NOT DISTINCT FROM 'captured')`},
	{5, "posted transfers post exactly the hold's captured amount", `
		SELECT 'transfer ' || t.id || ' posted ' || coalesce(e.debits, 0)
		       || ' but hold captured ' || coalesce(h.captured_amount::text, 'none')
		FROM transfers t
		JOIN holds h ON h.transfer_id = t.id
		LEFT JOIN (
			SELECT transfer_id, sum(amount) FILTER (WHERE direction = 'debit') AS debits
			FROM entries GROUP BY transfer_id
		) e ON e.transfer_id = t.id
		WHERE t.status = 'posted' AND coalesce(e.debits, 0) <> coalesce(h.captured_amount, -1)`},
	{7, "receivables never overpaid", `
		SELECT 'receivable account ' || a.id || ' posted=' || a.posted
		FROM accounts a WHERE a.subtype = 'receivable' AND a.posted < 0
		UNION ALL
		SELECT 'receivable account ' || e.account_id || ' went to ' || e.balance_after || ' at version ' || e.account_version
		FROM entries e JOIN accounts a ON a.id = e.account_id
		WHERE a.subtype = 'receivable' AND e.balance_after < 0`},
	{8, "no 2PC left in doubt", `
		SELECT 'prepared ' || gid || ' since ' || prepared
		FROM pg_prepared_xacts WHERE database = current_database()`},
}

type Result struct {
	Check
	Violations int
	Samples    []string // first few violation details
}

const maxSamples = 5

func (r *Result) add(detail string) {
	r.Violations++
	if len(r.Samples) < maxSamples {
		r.Samples = append(r.Samples, detail)
	}
}

// Run executes every check over the shards, in shard-index order. Pass one
// read-only REPEATABLE READ tx per shard (see Snapshot).
func Run(ctx context.Context, shards []store.Querier) ([]Result, error) {
	var results []Result
	for _, c := range Local {
		r := Result{Check: c}
		for i, q := range shards {
			rows, err := q.Query(ctx, c.SQL)
			if err != nil {
				return nil, fmt.Errorf("invariant %d (%s) on shard %d: %w", c.Invariant, c.Name, i, err)
			}
			details, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return nil, fmt.Errorf("invariant %d (%s) on shard %d: %w", c.Invariant, c.Name, i, err)
			}
			for _, d := range details {
				r.add(fmt.Sprintf("shard %d: %s", i, d))
			}
		}
		results = append(results, r)
	}
	global, err := runGlobal(ctx, shards)
	if err != nil {
		return nil, err
	}
	results = append(results, global...)
	slices.SortStableFunc(results, func(a, b Result) int { return a.Invariant - b.Invariant })
	return results, nil
}

// Snapshot reads every shard in its own read-only REPEATABLE READ tx and
// runs all checks. Together the snapshots are consistent only while the
// ledger is idle, so run it after the harness drains (A§9.6).
func Snapshot(ctx context.Context, shards *store.Shards) ([]Result, error) {
	var qs []store.Querier
	for i := range shards.N() {
		tx, err := shards.Pool(i).BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, fmt.Errorf("shard %d: %w", i, err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		qs = append(qs, tx)
	}
	return Run(ctx, qs)
}

// Clean reports whether no check found a violation.
func Clean(results []Result) bool {
	for _, r := range results {
		if r.Violations > 0 {
			return false
		}
	}
	return true
}
