package api

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	ledgerv1 "payment-ledger/gen/ledger/v1"
	"payment-ledger/internal/ledger"
)

func (s *Service) CreateTransfer(ctx context.Context, req *connect.Request[ledgerv1.CreateTransferRequest]) (*connect.Response[ledgerv1.CreateTransferResponse], error) {
	key, hash, err := idempotency(req)
	if err != nil {
		return nil, toConnect(err)
	}
	src, err := parseID("source_id", req.Msg.GetSourceId())
	if err != nil {
		return nil, err
	}
	dst, err := parseID("dest_id", req.Msg.GetDestId())
	if err != nil {
		return nil, err
	}
	var ttl time.Duration
	if d := req.Msg.GetHoldTtl(); d != nil {
		if ttl = d.AsDuration(); ttl <= 0 {
			return nil, ErrInvalidRequest("hold_ttl must be positive.")
		}
	}
	t, err := s.ledger.CreateTransfer(ctx, ledger.AcceptRequest{
		Key: key, Hash: hash, SourceID: src, DestID: dst, Amount: req.Msg.GetAmount(), HoldTTL: ttl,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.CreateTransferResponse{Transfer: transferPB(t)}), nil
}

func (s *Service) TopUp(ctx context.Context, req *connect.Request[ledgerv1.TopUpRequest]) (*connect.Response[ledgerv1.TopUpResponse], error) {
	key, hash, err := idempotency(req)
	if err != nil {
		return nil, toConnect(err)
	}
	wallet, err := parseID("wallet_id", req.Msg.GetWalletId())
	if err != nil {
		return nil, err
	}
	t, err := s.ledger.TopUp(ctx, key, hash, wallet, req.Msg.GetAmount())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.TopUpResponse{Transfer: transferPB(t)}), nil
}

func (s *Service) GetTransfer(ctx context.Context, req *connect.Request[ledgerv1.GetTransferRequest]) (*connect.Response[ledgerv1.GetTransferResponse], error) {
	id, err := parseID("id", req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	t, err := s.ledger.GetTransfer(ctx, id)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.GetTransferResponse{Transfer: transferPB(t)}), nil
}

func (s *Service) GetBalance(ctx context.Context, req *connect.Request[ledgerv1.GetBalanceRequest]) (*connect.Response[ledgerv1.GetBalanceResponse], error) {
	id, err := parseID("account_id", req.Msg.GetAccountId())
	if err != nil {
		return nil, err
	}
	b, err := s.ledger.GetBalance(ctx, id)
	if err != nil {
		return nil, toConnect(err)
	}
	resp := &ledgerv1.GetBalanceResponse{Posted: b.Posted, Available: b.Available, ReceivableOwed: b.ReceivableOwed}
	for _, h := range b.ActiveHolds {
		resp.ActiveHolds = append(resp.ActiveHolds, holdPB(h))
	}
	return connect.NewResponse(resp), nil
}

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, ErrInvalidRequest(field + " must be a UUID.")
	}
	return id, nil
}
