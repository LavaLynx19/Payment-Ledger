// Package checker verifies the A§4 ledger invariants with SQL. Every check
// returns one row per violation, as human-readable detail.
package checker

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/store"
)

type Check struct {
	Invariant int
	Name      string
	SQL       string // SELECT detail text, one row per violation
}

// Checks are the A§4 invariants 1-7.
var Checks = []Check{
	{1, "ledger balanced (Σ debits = Σ credits)", `
		SELECT 'ledger off by ' || s FROM (
			SELECT coalesce(sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END), 0) AS s FROM entries
		) x WHERE s <> 0`},
	{1, "each transfer balanced", `
		SELECT 'transfer ' || transfer_id || ' off by ' || sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END)
		FROM entries GROUP BY transfer_id
		HAVING sum(CASE WHEN direction = 'debit' THEN amount ELSE -amount END) <> 0`},
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
	{5, "posted transfers have a captured hold and entries; others have neither", `
		SELECT 'transfer ' || t.id || ' status=' || t.status || ' hold=' || coalesce(h.status, 'none') || ' entries=' || coalesce(e.n, 0)
		FROM transfers t
		LEFT JOIN holds h ON h.transfer_id = t.id
		LEFT JOIN (SELECT transfer_id, count(*) AS n FROM entries GROUP BY transfer_id) e ON e.transfer_id = t.id
		WHERE (t.status = 'posted') <> (h.status IS NOT DISTINCT FROM 'captured')
		   OR (t.status = 'posted') <> (coalesce(e.n, 0) > 0)`},
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
	// Several keys may name one Transfer (PlaceHold, then CaptureHold or ReleaseHold).
	{6, "each idempotency key maps to an existing transfer", `
		SELECT 'key ' || k.key || ' points at missing transfer ' || k.transfer_id
		FROM idempotency_keys k LEFT JOIN transfers t ON t.id = k.transfer_id
		WHERE t.id IS NULL`},
	{7, "reversals return exactly the posted amount, once", `
		SELECT 'transfer ' || o.id || ' posted ' || h.captured_amount || ' but reversals total '
		       || sum(r.amount) || ' across ' || count(*) || ' transfers'
		FROM transfers r
		JOIN transfers o ON o.id = r.reverses_id
		JOIN holds h ON h.transfer_id = o.id
		GROUP BY o.id, h.captured_amount
		HAVING sum(r.amount) <> h.captured_amount
		    OR count(*) FILTER (WHERE r.type = 'reversal') > 1
		    OR count(*) FILTER (WHERE r.type = 'receivable') > 1`},
	{7, "receivables never overpaid", `
		SELECT 'receivable account ' || a.id || ' posted=' || a.posted
		FROM accounts a WHERE a.subtype = 'receivable' AND a.posted < 0
		UNION ALL
		SELECT 'receivable account ' || e.account_id || ' went to ' || e.balance_after || ' at version ' || e.account_version
		FROM entries e JOIN accounts a ON a.id = e.account_id
		WHERE a.subtype = 'receivable' AND e.balance_after < 0`},
}

type Result struct {
	Check
	Violations int
	Samples    []string // first few violation details
}

const maxSamples = 5

// Run executes every check on q. Pass a REPEATABLE READ tx (see Snapshot) so
// all checks see one consistent state.
func Run(ctx context.Context, q store.Querier) ([]Result, error) {
	results := make([]Result, 0, len(Checks))
	for _, c := range Checks {
		rows, err := q.Query(ctx, c.SQL)
		if err != nil {
			return nil, fmt.Errorf("invariant %d (%s): %w", c.Invariant, c.Name, err)
		}
		details, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("invariant %d (%s): %w", c.Invariant, c.Name, err)
		}
		r := Result{Check: c, Violations: len(details)}
		r.Samples = details[:min(len(details), maxSamples)]
		results = append(results, r)
	}
	return results, nil
}

// Snapshot runs every check inside one read-only REPEATABLE READ tx.
func Snapshot(ctx context.Context, db interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}) ([]Result, error) {
	var results []Result
	err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) (err error) {
			results, err = Run(ctx, tx)
			return err
		})
	return results, err
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
