package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LockTransfer reads a Transfer and locks it until tx ends, so two
// reversals of it can't both proceed.
func LockTransfer(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Transfer, error) {
	t, err := scanTransfer(tx.QueryRow(ctx, `SELECT `+transferCols+` FROM transfers WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, ErrTransferNotFound
	}
	if err != nil {
		return Transfer{}, fmt.Errorf("lock transfer: %w", err)
	}
	return t, nil
}

// ReversalsOf returns the reversal and receivable Transfers created for
// reversedID. Either may be nil, and both are nil when it was never reversed.
func ReversalsOf(ctx context.Context, q Querier, reversedID uuid.UUID) (reversal, receivable *Transfer, err error) {
	rows, err := q.Query(ctx, `SELECT `+transferCols+` FROM transfers WHERE reverses_id = $1`, reversedID)
	if err != nil {
		return nil, nil, fmt.Errorf("reversals of: %w", err)
	}
	ts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Transfer, error) { return scanTransfer(r) })
	if err != nil {
		return nil, nil, fmt.Errorf("reversals of: %w", err)
	}
	for i := range ts {
		switch ts[i].Type {
		case "reversal":
			reversal = &ts[i]
		case "receivable":
			receivable = &ts[i]
		}
	}
	return reversal, receivable, nil
}

// ReceivableAccountID returns walletID's receivable System account, or
// ErrAccountNotFound if the Wallet has never been reversed against.
func ReceivableAccountID(ctx context.Context, q Querier, walletID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx,
		`SELECT id FROM accounts WHERE debtor_wallet_id = $1 AND subtype = 'receivable'`, walletID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrAccountNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("receivable account: %w", err)
	}
	return id, nil
}

// PendingInto is the amount active Holds are moving into accountID, i.e.
// accepted Repayments not yet captured.
func PendingInto(ctx context.Context, q Querier, accountID uuid.UUID) (int64, error) {
	var total int64
	err := q.QueryRow(ctx,
		`SELECT coalesce(sum(amount), 0) FROM holds WHERE dest_id = $1 AND `+activeHold, accountID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("pending into: %w", err)
	}
	return total, nil
}

// EnsureReceivableAccount returns walletID's receivable System account,
// creating it on first use. It's debit-normal: posted is what the debtor owes.
func EnsureReceivableAccount(ctx context.Context, tx pgx.Tx, walletID uuid.UUID) (Account, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Account{}, err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO accounts (id, kind, subtype, debtor_wallet_id, normal_balance)
		 VALUES ($1, 'system', 'receivable', $2, 'debit')
		 ON CONFLICT (debtor_wallet_id) WHERE subtype = 'receivable' DO NOTHING`, id, walletID)
	if err != nil {
		return Account{}, fmt.Errorf("create receivable account: %w", err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT id FROM accounts WHERE debtor_wallet_id = $1 AND subtype = 'receivable'`, walletID,
	).Scan(&id); err != nil {
		return Account{}, fmt.Errorf("find receivable account: %w", err)
	}
	return GetAccount(ctx, tx, id)
}
