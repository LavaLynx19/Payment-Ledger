// Package api is the connect-go transport: handlers, auth, and the A§7 error
// mapping. Ledger logic lives in internal/ledger.
package api

import (
	"net/http"

	"connectrpc.com/connect"

	"payment-ledger/gen/ledger/v1/ledgerv1connect"
)

// Service implements LedgerService on top of an Engine.
type Service struct {
	ledgerv1connect.UnimplementedLedgerServiceHandler
	engine Engine
}

func NewService(e Engine) *Service { return &Service{engine: e} }

// NewHandler mounts svc behind the auth interceptor. The handler serves gRPC,
// gRPC-Web, and Connect HTTP/JSON.
func NewHandler(svc *Service, tokens []string) http.Handler {
	mux := http.NewServeMux()
	path, h := ledgerv1connect.NewLedgerServiceHandler(svc,
		connect.WithInterceptors(NewAuthInterceptor(tokens)))
	mux.Handle(path, h)
	return mux
}
