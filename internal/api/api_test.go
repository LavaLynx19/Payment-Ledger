package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	ledgerv1 "payment-ledger/gen/ledger/v1"
	"payment-ledger/gen/ledger/v1/ledgerv1connect"
)

func TestErrorTable(t *testing.T) {
	cases := []struct {
		err    *connect.Error
		code   connect.Code
		reason ledgerv1.ErrorDetail_Reason
	}{
		{ErrInsufficientFunds(10, 20), connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_INSUFFICIENT_FUNDS},
		{ErrReceivableOpen(80), connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_RECEIVABLE_OPEN},
		{ErrHoldExpired(time.Now()), connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_HOLD_EXPIRED},
		{ErrHoldNotActive("released"), connect.CodeFailedPrecondition, ledgerv1.ErrorDetail_REASON_HOLD_NOT_ACTIVE},
		{ErrIdempotencyMismatch(), connect.CodeAlreadyExists, ledgerv1.ErrorDetail_REASON_IDEMPOTENCY_MISMATCH},
		{ErrConflictRetriesExhausted(), connect.CodeAborted, ledgerv1.ErrorDetail_REASON_CONFLICT_RETRIES_EXHAUSTED},
		{ErrNotFound("transfer", "x"), connect.CodeNotFound, ledgerv1.ErrorDetail_REASON_NOT_FOUND},
		{ErrUnauthenticated(), connect.CodeUnauthenticated, ledgerv1.ErrorDetail_REASON_UNAUTHENTICATED},
	}
	for _, c := range cases {
		if c.err.Code() != c.code {
			t.Errorf("%s: code = %v, want %v", c.reason, c.err.Code(), c.code)
		}
		if got := reasonOf(t, c.err); got != c.reason {
			t.Errorf("reason = %v, want %v", got, c.reason)
		}
	}
}

func TestAuth(t *testing.T) {
	srv := httptest.NewServer(NewHandler(&Service{}, []string{"good"}))
	defer srv.Close()
	client := ledgerv1connect.NewLedgerServiceClient(http.DefaultClient, srv.URL)

	call := func(token string) error {
		req := connect.NewRequest(&ledgerv1.GetTransferRequest{Id: "x"})
		if token != "" {
			req.Header().Set("Authorization", "Bearer "+token)
		}
		_, err := client.GetTransfer(context.Background(), req)
		return err
	}

	for _, tok := range []string{"", "bad"} {
		if code := connect.CodeOf(call(tok)); code != connect.CodeUnauthenticated {
			t.Errorf("token %q: code = %v, want unauthenticated", tok, code)
		}
	}
	// A valid token passes auth and reaches the not-yet-built handler.
	if code := connect.CodeOf(call("good")); code != connect.CodeUnimplemented {
		t.Errorf("valid token: code = %v, want unimplemented", code)
	}
}

func reasonOf(t *testing.T, err *connect.Error) ledgerv1.ErrorDetail_Reason {
	t.Helper()
	for _, d := range err.Details() {
		v, derr := d.Value()
		if derr != nil {
			t.Fatal(derr)
		}
		if ed, ok := v.(*ledgerv1.ErrorDetail); ok {
			return ed.GetReason()
		}
	}
	return ledgerv1.ErrorDetail_REASON_UNSPECIFIED
}
