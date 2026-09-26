package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var ErrTransferNotFound = errors.New("store: transfer not found")

type Transfer struct {
	ID         uuid.UUID
	Type       string
	SourceID   uuid.UUID
	DestID     uuid.UUID
	Amount     int64
	Status     string
	ReversesID *uuid.UUID
	CreatedAt  time.Time
	PostedAt   *time.Time
}

type Hold struct {
	ID             uuid.UUID
	TransferID     uuid.UUID
	SourceID       uuid.UUID
	DestID         uuid.UUID
	Amount         int64
	CapturedAmount *int64
	ExpiresAt      *time.Time // nil = never expires
	Status         string
}

type Entry struct {
	TransferID     uuid.UUID
	AccountID      uuid.UUID
	Direction      string
	Amount         int64
	BalanceAfter   int64
	AccountVersion int64
}

const transferCols = `id, type, source_id, dest_id, amount, status, reverses_id, created_at, posted_at`
const holdCols = `id, transfer_id, source_id, dest_id, amount, captured_amount, expires_at, status`

// activeHold matches Holds that still reserve funds: active and not past expiry
// (A§4 lazy expiry).
const activeHold = `status = 'active' AND (expires_at IS NULL OR expires_at > clock_timestamp())`

func scanTransfer(row pgx.Row) (Transfer, error) {
	var t Transfer
	err := row.Scan(&t.ID, &t.Type, &t.SourceID, &t.DestID, &t.Amount, &t.Status, &t.ReversesID, &t.CreatedAt, &t.PostedAt)
	return t, err
}

func scanHold(row pgx.Row) (Hold, error) {
	var h Hold
	err := row.Scan(&h.ID, &h.TransferID, &h.SourceID, &h.DestID, &h.Amount, &h.CapturedAmount, &h.ExpiresAt, &h.Status)
	return h, err
}

func GetTransfer(ctx context.Context, q Querier, id uuid.UUID) (Transfer, error) {
	t, err := scanTransfer(q.QueryRow(ctx, `SELECT `+transferCols+` FROM transfers WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, ErrTransferNotFound
	}
	if err != nil {
		return Transfer{}, fmt.Errorf("get transfer: %w", err)
	}
	return t, nil
}

// InsertPendingTransfer creates t as a pending Transfer together with its
// active Hold. The Hold expires holdTTL after now (database clock), or never
// when holdTTL is 0.
func InsertPendingTransfer(ctx context.Context, tx pgx.Tx, t Transfer, holdTTL time.Duration) (Transfer, error) {
	out, err := scanTransfer(tx.QueryRow(ctx,
		`INSERT INTO transfers (id, type, source_id, dest_id, amount, status, reverses_id)
		 VALUES ($1, $2, $3, $4, $5, 'pending', $6)
		 RETURNING `+transferCols,
		t.ID, t.Type, t.SourceID, t.DestID, t.Amount, t.ReversesID))
	if err != nil {
		return Transfer{}, fmt.Errorf("insert transfer: %w", err)
	}

	holdID, err := uuid.NewV7()
	if err != nil {
		return Transfer{}, err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO holds (id, transfer_id, source_id, dest_id, amount, status, expires_at)
		 VALUES ($1, $2, $3, $4, $5, 'active',
		         CASE WHEN $6::bigint = 0 THEN NULL
		              ELSE clock_timestamp() + $6::bigint * interval '1 microsecond' END)`,
		holdID, t.ID, t.SourceID, t.DestID, t.Amount, holdTTL.Microseconds())
	if err != nil {
		return Transfer{}, fmt.Errorf("insert hold: %w", err)
	}
	return out, nil
}

// ActiveHoldTotal is the amount the account's active Holds reserve.
func ActiveHoldTotal(ctx context.Context, q Querier, accountID uuid.UUID) (int64, error) {
	var total int64
	err := q.QueryRow(ctx,
		`SELECT coalesce(sum(amount), 0) FROM holds WHERE source_id = $1 AND `+activeHold, accountID,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("active hold total: %w", err)
	}
	return total, nil
}

func ActiveHolds(ctx context.Context, q Querier, accountID uuid.UUID) ([]Hold, error) {
	rows, err := q.Query(ctx,
		`SELECT `+holdCols+` FROM holds WHERE source_id = $1 AND `+activeHold+` ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("active holds: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hold, error) { return scanHold(r) })
}

// ClaimCapturableHold locks the oldest active Hold that no other worker holds
// (FOR UPDATE SKIP LOCKED). It reports false when none is available.
func ClaimCapturableHold(ctx context.Context, tx pgx.Tx) (Hold, bool, error) {
	h, err := scanHold(tx.QueryRow(ctx,
		`SELECT `+holdCols+` FROM holds WHERE `+activeHold+`
		 ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, false, nil
	}
	if err != nil {
		return Hold{}, false, fmt.Errorf("claim hold: %w", err)
	}
	return h, true, nil
}

// MarkHoldCaptured moves an active, unexpired Hold to captured. It reports
// false when expiry won the race (A§5 Capture step 2).
func MarkHoldCaptured(ctx context.Context, tx pgx.Tx, holdID uuid.UUID, amount int64) (bool, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE holds SET status = 'captured', captured_amount = $2
		 WHERE id = $1 AND `+activeHold, holdID, amount)
	if err != nil {
		return false, fmt.Errorf("capture hold: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// InsertEntry records e, stamped with clock_timestamp() after the Account CAS
// that produced its balance and version.
func InsertEntry(ctx context.Context, tx pgx.Tx, e Entry) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO entries (id, transfer_id, account_id, direction, amount, balance_after, account_version, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, clock_timestamp())`,
		id, e.TransferID, e.AccountID, e.Direction, e.Amount, e.BalanceAfter, e.AccountVersion)
	if err != nil {
		return fmt.Errorf("insert entry: %w", err)
	}
	return nil
}

// SetTransferStatus sets status. Moving to 'posted' also stamps posted_at.
func SetTransferStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status string) error {
	_, err := tx.Exec(ctx,
		`UPDATE transfers SET status = $2,
		        posted_at = CASE WHEN $2 = 'posted' THEN clock_timestamp() ELSE posted_at END
		 WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("set transfer status: %w", err)
	}
	return nil
}

// ReceivableOwed is what walletID owes on its receivable System account,
// including shortfalls whose Holds are not yet captured (A§4). It is 0 when
// the Wallet has no receivable account.
func ReceivableOwed(ctx context.Context, q Querier, walletID uuid.UUID) (int64, error) {
	var owed int64
	err := q.QueryRow(ctx,
		`SELECT coalesce(sum(a.posted + coalesce(
		            (SELECT sum(h.amount) FROM holds h WHERE h.source_id = a.id AND h.status = 'active'), 0)), 0)
		 FROM accounts a WHERE a.debtor_wallet_id = $1`, walletID,
	).Scan(&owed)
	if err != nil {
		return 0, fmt.Errorf("receivable owed: %w", err)
	}
	return owed, nil
}

// FundingAccountID returns the funding System account created by cmd/seed.
func FundingAccountID(ctx context.Context, q Querier) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM accounts WHERE subtype = 'funding' LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrAccountNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("funding account: %w", err)
	}
	return id, nil
}
