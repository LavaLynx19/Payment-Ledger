package ledger

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payment-ledger/internal/shard"
	"payment-ledger/internal/store"
)

func (l *Ledger) GetTransfer(ctx context.Context, id uuid.UUID) (store.Transfer, error) {
	t, err := store.GetTransfer(ctx, l.shards.For(id), id)
	if errors.Is(err, store.ErrTransferNotFound) {
		return store.Transfer{}, &NotFoundError{Resource: "transfer", ID: id.String()}
	}
	return t, err
}

// MaxPage caps every list read.
const MaxPage = 1000

// GetBalanceAt is the Account's posted balance as of at (A§7 read semantics).
func (l *Ledger) GetBalanceAt(ctx context.Context, accountID uuid.UUID, at time.Time) (int64, error) {
	if err := l.exists(ctx, accountID); err != nil {
		return 0, err
	}
	return store.BalanceAt(ctx, l.shards.For(accountID), accountID, at)
}

// ListEntries pages an Account's Entries in version order. afterVersion is 0
// for the first page. More is true when another page may follow.
func (l *Ledger) ListEntries(ctx context.Context, accountID uuid.UUID, from, to *time.Time, afterVersion int64, limit int) (entries []store.EntryRow, more bool, err error) {
	if err := l.exists(ctx, accountID); err != nil {
		return nil, false, err
	}
	limit = pageSize(limit)
	entries, err = store.ListEntries(ctx, l.shards.For(accountID), accountID, from, to, afterVersion, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}

// ListReceivables pages open Receivables by debtor id across every shard.
// afterDebtor is uuid.Nil for the first page. A minAge of 0 lists all of them.
//
// Each shard returns its next limit+1 debtors after the cursor in id order.
// Merging those by debtor id and keeping the first limit+1 gives an exact
// global page: no debtor beyond a shard's limit+1 can sort before the page
// end (A§9.6).
func (l *Ledger) ListReceivables(ctx context.Context, minAge time.Duration, afterDebtor uuid.UUID, limit int) ([]store.Receivable, bool, error) {
	var openedBefore *time.Time
	if minAge > 0 {
		now, err := store.Now(ctx, l.shards.Pool(0)) // all shards share one host clock
		if err != nil {
			return nil, false, err
		}
		cut := now.Add(-minAge)
		openedBefore = &cut
	}
	limit = pageSize(limit)
	var rs []store.Receivable
	for i := range l.shards.N() {
		part, err := store.ListReceivables(ctx, l.shards.Pool(i), afterDebtor, openedBefore, limit+1)
		if err != nil {
			return nil, false, err
		}
		rs = append(rs, part...)
	}
	slices.SortFunc(rs, func(a, b store.Receivable) int {
		return bytes.Compare(a.DebtorWalletID[:], b.DebtorWalletID[:]) // Postgres uuid order
	})
	if len(rs) > limit {
		return rs[:limit], true, nil
	}
	return rs, false, nil
}

func pageSize(limit int) int {
	if limit <= 0 {
		return 100
	}
	return min(limit, MaxPage)
}

// shardOf is the index of the shard that stores id.
func (l *Ledger) shardOf(id uuid.UUID) int { return shard.Route(id, l.shards.N()) }

func (l *Ledger) exists(ctx context.Context, accountID uuid.UUID) error {
	var ok bool
	if err := l.shards.For(accountID).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = $1)`, accountID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return &NotFoundError{Resource: "account", ID: accountID.String()}
	}
	return nil
}

type Balance struct {
	Posted         int64
	Available      int64
	ActiveHolds    []store.Hold
	ReceivableOwed int64
}

// GetBalance reads posted, Available, active Holds and Receivable from one
// snapshot, so the numbers agree with each other.
func (l *Ledger) GetBalance(ctx context.Context, accountID uuid.UUID) (Balance, error) {
	var b Balance
	// A receivable shares its debtor's shard, so one snapshot covers it all.
	err := pgx.BeginTxFunc(ctx, l.shards.For(accountID), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			a, err := l.account(ctx, tx, accountID)
			if err != nil {
				return err
			}
			if b.ActiveHolds, err = store.ActiveHolds(ctx, tx, accountID); err != nil {
				return err
			}
			if b.ReceivableOwed, err = store.ReceivableOwed(ctx, tx, accountID); err != nil {
				return err
			}
			b.Posted, b.Available = a.Posted, a.Posted
			for _, h := range b.ActiveHolds {
				b.Available -= h.Amount
			}
			return nil
		})
	return b, err
}
