package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type EntryRow struct {
	Entry
	ID        uuid.UUID
	CreatedAt time.Time
}

type Receivable struct {
	DebtorWalletID      uuid.UUID
	ReceivableAccountID uuid.UUID
	Owed                int64
	OpenedAt            time.Time
}

// BalanceAt is the account's posted balance as of at: balance_after of its
// latest Entry at or before at, ties broken by version. It's 0 before the
// first Entry.
func BalanceAt(ctx context.Context, q Querier, accountID uuid.UUID, at time.Time) (int64, error) {
	var bal int64
	err := q.QueryRow(ctx,
		`SELECT balance_after FROM entries
		 WHERE account_id = $1 AND created_at <= $2
		 ORDER BY created_at DESC, account_version DESC LIMIT 1`, accountID, at).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("balance at: %w", err)
	}
	return bal, nil
}

// ListEntries returns up to limit Entries of accountID after version
// afterVersion, in version order, optionally bounded by from ≤ created_at < to
// (nil = unbounded).
func ListEntries(ctx context.Context, q Querier, accountID uuid.UUID, from, to *time.Time, afterVersion int64, limit int) ([]EntryRow, error) {
	rows, err := q.Query(ctx,
		`SELECT id, transfer_id, account_id, direction, amount, balance_after, account_version, created_at
		 FROM entries
		 WHERE account_id = $1 AND account_version > $2
		   AND ($3::timestamptz IS NULL OR created_at >= $3)
		   AND ($4::timestamptz IS NULL OR created_at < $4)
		 ORDER BY account_version LIMIT $5`, accountID, afterVersion, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("list entries: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EntryRow, error) {
		var e EntryRow
		err := r.Scan(&e.ID, &e.TransferID, &e.AccountID, &e.Direction, &e.Amount, &e.BalanceAfter, &e.AccountVersion, &e.CreatedAt)
		return e, err
	})
}

// ListReceivables returns up to limit open Receivables (owed > 0, counting
// uncaptured shortfalls) with debtor id after afterDebtor, ordered by debtor.
// openedBefore, when set, keeps only those opened at or before it.
func ListReceivables(ctx context.Context, q Querier, afterDebtor uuid.UUID, openedBefore *time.Time, limit int) ([]Receivable, error) {
	rows, err := q.Query(ctx,
		`WITH owed AS (
		     SELECT a.id, a.debtor_wallet_id,
		            a.posted + coalesce((SELECT sum(h.amount) FROM holds h
		                                 WHERE h.source_id = a.id AND h.status = 'active'), 0) AS owed
		     FROM accounts a
		     WHERE a.subtype = 'receivable' AND a.debtor_wallet_id > $1
		 ), open AS (
		     SELECT o.*,
		            (SELECT min(t.created_at) FROM transfers t
		             WHERE t.source_id = o.id AND t.type = 'receivable'
		               AND t.created_at > coalesce(
		                   (SELECT max(e.created_at) FROM entries e
		                    WHERE e.account_id = o.id AND e.balance_after = 0), '-infinity')) AS opened_at
		     FROM owed o WHERE o.owed > 0
		 )
		 SELECT debtor_wallet_id, id, owed, opened_at FROM open
		 WHERE $2::timestamptz IS NULL OR opened_at <= $2
		 ORDER BY debtor_wallet_id LIMIT $3`, afterDebtor, openedBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("list receivables: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Receivable, error) {
		var rc Receivable
		err := r.Scan(&rc.DebtorWalletID, &rc.ReceivableAccountID, &rc.Owed, &rc.OpenedAt)
		return rc, err
	})
}

// Now is the database clock, which stamps every Entry. Callers use it so
// time filters line up with created_at.
func Now(ctx context.Context, q Querier) (time.Time, error) {
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("db clock: %w", err)
	}
	return now, nil
}
