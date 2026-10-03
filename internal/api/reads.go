package api

import (
	"context"
	"encoding/base64"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "payment-ledger/gen/ledger/v1"
)

func (s *Service) GetBalanceAt(ctx context.Context, req *connect.Request[ledgerv1.GetBalanceAtRequest]) (*connect.Response[ledgerv1.GetBalanceAtResponse], error) {
	id, err := parseID("account_id", req.Msg.GetAccountId())
	if err != nil {
		return nil, err
	}
	if req.Msg.GetAt() == nil {
		return nil, ErrInvalidRequest("at is required.")
	}
	posted, err := s.engine.GetBalanceAt(ctx, id, req.Msg.GetAt().AsTime())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&ledgerv1.GetBalanceAtResponse{Posted: posted}), nil
}

func (s *Service) ListEntries(ctx context.Context, req *connect.Request[ledgerv1.ListEntriesRequest]) (*connect.Response[ledgerv1.ListEntriesResponse], error) {
	id, err := parseID("account_id", req.Msg.GetAccountId())
	if err != nil {
		return nil, err
	}
	var after int64
	if c := req.Msg.GetCursor(); c != "" {
		raw, derr := base64.RawURLEncoding.DecodeString(c)
		if after, err = strconv.ParseInt(string(raw), 10, 64); derr != nil || err != nil || after < 0 {
			return nil, ErrInvalidRequest("cursor is not valid.")
		}
	}
	entries, more, err := s.engine.ListEntries(ctx, id,
		optionalTimestamp(req.Msg.GetFrom()), optionalTimestamp(req.Msg.GetTo()), after, int(req.Msg.GetLimit()))
	if err != nil {
		return nil, toConnect(err)
	}
	resp := &ledgerv1.ListEntriesResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &ledgerv1.Entry{
			Id:             e.ID.String(),
			TransferId:     e.TransferID.String(),
			AccountId:      e.AccountID.String(),
			Direction:      directions[e.Direction],
			Amount:         e.Amount,
			BalanceAfter:   e.BalanceAfter,
			AccountVersion: e.AccountVersion,
			CreatedAt:      timestamppb.New(e.CreatedAt),
		})
	}
	if more {
		last := entries[len(entries)-1].AccountVersion
		resp.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(last, 10)))
	}
	return connect.NewResponse(resp), nil
}

func (s *Service) ListReceivables(ctx context.Context, req *connect.Request[ledgerv1.ListReceivablesRequest]) (*connect.Response[ledgerv1.ListReceivablesResponse], error) {
	after := uuid.Nil
	if c := req.Msg.GetCursor(); c != "" {
		raw, derr := base64.RawURLEncoding.DecodeString(c)
		var perr error
		if after, perr = uuid.ParseBytes(raw); derr != nil || perr != nil {
			return nil, ErrInvalidRequest("cursor is not valid.")
		}
	}
	var minAge time.Duration
	if d := req.Msg.GetMinAge(); d != nil {
		if minAge = d.AsDuration(); minAge < 0 {
			return nil, ErrInvalidRequest("min_age must not be negative.")
		}
	}
	rs, more, err := s.engine.ListReceivables(ctx, minAge, after, int(req.Msg.GetLimit()))
	if err != nil {
		return nil, toConnect(err)
	}
	resp := &ledgerv1.ListReceivablesResponse{}
	for _, r := range rs {
		resp.Receivables = append(resp.Receivables, &ledgerv1.Receivable{
			DebtorWalletId:      r.DebtorWalletID.String(),
			ReceivableAccountId: r.ReceivableAccountID.String(),
			Owed:                r.Owed,
			OpenedAt:            timestamppb.New(r.OpenedAt),
		})
	}
	if more {
		resp.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(rs[len(rs)-1].DebtorWalletID.String()))
	}
	return connect.NewResponse(resp), nil
}

var directions = map[string]ledgerv1.Direction{
	"debit":  ledgerv1.Direction_DIRECTION_DEBIT,
	"credit": ledgerv1.Direction_DIRECTION_CREDIT,
}

func optionalTimestamp(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
