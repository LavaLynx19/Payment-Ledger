package api

import (
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	ledgerv1 "payment-ledger/gen/ledger/v1"
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

func ErrUnauthenticated() *connect.Error {
	return newError(connect.CodeUnauthenticated, ledgerv1.ErrorDetail_REASON_UNAUTHENTICATED,
		"Missing or invalid service token.")
}

func newError(code connect.Code, reason ledgerv1.ErrorDetail_Reason, msg string) *connect.Error {
	err := connect.NewError(code, errors.New(msg))
	if d, derr := connect.NewErrorDetail(&ledgerv1.ErrorDetail{Reason: reason}); derr == nil {
		err.AddDetail(d)
	}
	return err
}
