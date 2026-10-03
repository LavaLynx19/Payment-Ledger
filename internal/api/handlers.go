package api

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/durationpb"

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
	ttl, err := optionalTTL("hold_ttl", req.Msg.GetHoldTtl())
	if err != nil {
		return nil, err
	}
	t, err := s.engine.CreateTransfer(ctx, ledger.AcceptRequest{
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
	t, err := s.engine.TopUp(ctx, key, hash, wallet, req.Msg.GetAmount())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.TopUpResponse{Transfer: transferPB(t)}), nil
}

func (s *Service) Withdraw(ctx context.Context, req *connect.Request[ledgerv1.WithdrawRequest]) (*connect.Response[ledgerv1.WithdrawResponse], error) {
	key, hash, err := idempotency(req)
	if err != nil {
		return nil, toConnect(err)
	}
	wallet, err := parseID("wallet_id", req.Msg.GetWalletId())
	if err != nil {
		return nil, err
	}
	t, err := s.engine.Withdraw(ctx, key, hash, wallet, req.Msg.GetAmount())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.WithdrawResponse{Transfer: transferPB(t)}), nil
}

func (s *Service) ReverseTransfer(ctx context.Context, req *connect.Request[ledgerv1.ReverseTransferRequest]) (*connect.Response[ledgerv1.ReverseTransferResponse], error) {
	key, hash, err := idempotency(req)
	if err != nil {
		return nil, toConnect(err)
	}
	id, err := parseID("transfer_id", req.Msg.GetTransferId())
	if err != nil {
		return nil, err
	}
	reversal, receivable, err := s.engine.ReverseTransfer(ctx, key, hash, id)
	if err != nil {
		return nil, toConnect(err)
	}
	resp := &ledgerv1.ReverseTransferResponse{}
	if reversal != nil {
		resp.Reversal = transferPB(*reversal)
	}
	if receivable != nil {
		resp.Receivable = transferPB(*receivable)
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) Repay(ctx context.Context, req *connect.Request[ledgerv1.RepayRequest]) (*connect.Response[ledgerv1.RepayResponse], error) {
	key, hash, err := idempotency(req)
	if err != nil {
		return nil, toConnect(err)
	}
	wallet, err := parseID("wallet_id", req.Msg.GetWalletId())
	if err != nil {
		return nil, err
	}
	t, err := s.engine.Repay(ctx, key, hash, wallet, req.Msg.GetAmount())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.RepayResponse{Transfer: transferPB(t)}), nil
}

func (s *Service) GetTransfer(ctx context.Context, req *connect.Request[ledgerv1.GetTransferRequest]) (*connect.Response[ledgerv1.GetTransferResponse], error) {
	id, err := parseID("id", req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	t, err := s.engine.GetTransfer(ctx, id)
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
	b, err := s.engine.GetBalance(ctx, id)
	if err != nil {
		return nil, toConnect(err)
	}
	resp := &ledgerv1.GetBalanceResponse{Posted: b.Posted, Available: b.Available, ReceivableOwed: b.ReceivableOwed}
	for _, h := range b.ActiveHolds {
		resp.ActiveHolds = append(resp.ActiveHolds, holdPB(h))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) PlaceHold(ctx context.Context, req *connect.Request[ledgerv1.PlaceHoldRequest]) (*connect.Response[ledgerv1.PlaceHoldResponse], error) {
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
	ttl, err := optionalTTL("ttl", req.Msg.GetTtl())
	if err != nil {
		return nil, err
	}
	t, h, err := s.engine.PlaceHold(ctx, ledger.AcceptRequest{
		Key: key, Hash: hash, SourceID: src, DestID: dst, Amount: req.Msg.GetAmount(), HoldTTL: ttl,
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.PlaceHoldResponse{Hold: holdPB(h), Transfer: transferPB(t)}), nil
}

func (s *Service) CaptureHold(ctx context.Context, req *connect.Request[ledgerv1.CaptureHoldRequest]) (*connect.Response[ledgerv1.CaptureHoldResponse], error) {
	key, hash, err := idempotency(req)
	if err != nil {
		return nil, toConnect(err)
	}
	holdID, err := parseID("hold_id", req.Msg.GetHoldId())
	if err != nil {
		return nil, err
	}
	t, h, err := s.engine.CaptureHold(ctx, key, hash, holdID, req.Msg.Amount)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.CaptureHoldResponse{Hold: holdPB(h), Transfer: transferPB(t)}), nil
}

func (s *Service) ReleaseHold(ctx context.Context, req *connect.Request[ledgerv1.ReleaseHoldRequest]) (*connect.Response[ledgerv1.ReleaseHoldResponse], error) {
	key, hash, err := idempotency(req)
	if err != nil {
		return nil, toConnect(err)
	}
	holdID, err := parseID("hold_id", req.Msg.GetHoldId())
	if err != nil {
		return nil, err
	}
	h, err := s.engine.ReleaseHold(ctx, key, hash, holdID)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.ReleaseHoldResponse{Hold: holdPB(h)}), nil
}

// optionalTTL returns 0 (use the server default) when d is unset.
func optionalTTL(field string, d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	ttl := d.AsDuration()
	if ttl <= 0 {
		return 0, ErrInvalidRequest(field + " must be positive.")
	}
	return ttl, nil
}

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, ErrInvalidRequest(field + " must be a UUID.")
	}
	return id, nil
}
