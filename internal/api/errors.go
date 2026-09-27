package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"connectrpc.com/connect"

	ledgerv1 "payment-ledger/gen/ledger/v1"
	"payment-ledger/internal/ledger"
)

// Constructors for the A§7 error table. Each error carries a Connect code, an
// ErrorDetail reason, and the human-readable message.

func ErrInsufficientFunds(available, amount int64) *connect.Error {
	return newError(connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_INSUFFICIENT_FUNDS,
		fmt.Sprintf("Available balance is %d, which is less than the %d requested.", available, amount))
}

func ErrReceivableOpen(owed int64) *connect.Error {
	return newError(connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_RECEIVABLE_OPEN,
		fmt.Sprintf("This wallet owes %d from a reversed transfer. Only repayments are allowed until it's paid off.", owed))
}

func ErrHoldExpired(expiresAt time.Time) *connect.Error {
	return newError(connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_HOLD_EXPIRED,
		fmt.Sprintf("This hold expired at %s and can no longer be captured.", expiresAt.UTC().Format(time.RFC3339)))
}

func ErrHoldNotActive(status string) *connect.Error {
	return newError(connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_HOLD_NOT_ACTIVE,
		fmt.Sprintf("This hold has already been %s.", status))
}

func ErrIdempotencyMismatch() *connect.Error {
	return newError(connect.CodeAlreadyExists, ledgerv1.ErrorDetail_REASON_IDEMPOTENCY_MISMATCH,
		"This Idempotency-Key was already used with a different request. Use a new key for a new request.")
}

func ErrConflictRetriesExhausted() *connect.Error {
	return newError(connect.CodeAborted, ledgerv1.ErrorDetail_REASON_CONFLICT_RETRIES_EXHAUSTED,
		"The account was busy and the request couldn't be applied. Retry with the same Idempotency-Key.")
}

func ErrNotFound(resource, id string) *connect.Error {
	return newError(connect.CodeNotFound, ledgerv1.ErrorDetail_REASON_NOT_FOUND,
		fmt.Sprintf("No %s found with id %s.", resource, id))
}

// ErrInvalidRequest takes a message that names the field and the rule it
// broke, e.g. "amount must be greater than 0."
func ErrInvalidRequest(msg string) *connect.Error {
	return newError(connect.CodeInvalidArgument, ledgerv1.ErrorDetail_REASON_INVALID_REQUEST, msg)
}

// ErrTransferNotReversible takes a message that names the reason, e.g. "This
// transfer has already been reversed."
func ErrTransferNotReversible(msg string) *connect.Error {
	return newError(connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_TRANSFER_NOT_REVERSIBLE, msg)
}

func ErrUnauthenticated() *connect.Error {
	return newError(connect.CodeUnauthenticated, ledgerv1.ErrorDetail_REASON_UNAUTHENTICATED,
		"Missing or invalid service token.")
}

// toConnect maps a domain error to its A§7 error. Anything unrecognized is
// logged and returned as a generic internal error, so no internals leak.
func toConnect(err error) error {
	var (
		cerr         *connect.Error
		insufficient *ledger.InsufficientFundsError
		notFound     *ledger.NotFoundError
		inv          *ledger.InvalidError
		notActive    *ledger.HoldNotActiveError
		expired      *ledger.HoldExpiredError
		irreversible *ledger.NotReversibleError
		owes         *ledger.ReceivableOpenError
	)
	switch {
	case errors.As(err, &cerr):
		return cerr
	case errors.As(err, &owes):
		return ErrReceivableOpen(owes.Owed)
	case errors.As(err, &irreversible):
		return ErrTransferNotReversible(irreversible.Msg)
	case errors.As(err, &insufficient):
		return ErrInsufficientFunds(insufficient.Available, insufficient.Amount)
	case errors.As(err, &notActive):
		return ErrHoldNotActive(notActive.Status)
	case errors.As(err, &expired):
		return ErrHoldExpired(expired.ExpiresAt)
	case errors.As(err, &notFound):
		return ErrNotFound(notFound.Resource, notFound.ID)
	case errors.As(err, &inv):
		return ErrInvalidRequest(inv.Msg)
	case errors.Is(err, ledger.ErrIdempotencyMismatch):
		return ErrIdempotencyMismatch()
	case errors.Is(err, ledger.ErrRetriesExhausted):
		return ErrConflictRetriesExhausted()
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	log.Printf("api: internal error: %v", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal error"))
}

func newError(code connect.Code, reason ledgerv1.ErrorDetail_Reason, msg string) *connect.Error {
	err := connect.NewError(code, errors.New(msg))
	if d, derr := connect.NewErrorDetail(&ledgerv1.ErrorDetail{Reason: reason}); derr == nil {
		err.AddDetail(d)
	}
	return err
}
