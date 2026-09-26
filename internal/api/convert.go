package api

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "payment-ledger/gen/ledger/v1"
	"payment-ledger/internal/store"
)

var transferTypes = map[string]ledgerv1.TransferType{
	"p2p":        ledgerv1.TransferType_TRANSFER_TYPE_P2P,
	"topup":      ledgerv1.TransferType_TRANSFER_TYPE_TOPUP,
	"withdrawal": ledgerv1.TransferType_TRANSFER_TYPE_WITHDRAWAL,
	"reversal":   ledgerv1.TransferType_TRANSFER_TYPE_REVERSAL,
	"repayment":  ledgerv1.TransferType_TRANSFER_TYPE_REPAYMENT,
	"receivable": ledgerv1.TransferType_TRANSFER_TYPE_RECEIVABLE,
}

var transferStatuses = map[string]ledgerv1.TransferStatus{
	"pending": ledgerv1.TransferStatus_TRANSFER_STATUS_PENDING,
	"posted":  ledgerv1.TransferStatus_TRANSFER_STATUS_POSTED,
	"failed":  ledgerv1.TransferStatus_TRANSFER_STATUS_FAILED,
}

var holdStatuses = map[string]ledgerv1.HoldStatus{
	"active":   ledgerv1.HoldStatus_HOLD_STATUS_ACTIVE,
	"captured": ledgerv1.HoldStatus_HOLD_STATUS_CAPTURED,
	"released": ledgerv1.HoldStatus_HOLD_STATUS_RELEASED,
	"expired":  ledgerv1.HoldStatus_HOLD_STATUS_EXPIRED,
}

func transferPB(t store.Transfer) *ledgerv1.Transfer {
	pb := &ledgerv1.Transfer{
		Id:        t.ID.String(),
		Type:      transferTypes[t.Type],
		SourceId:  t.SourceID.String(),
		DestId:    t.DestID.String(),
		Amount:    t.Amount,
		Status:    transferStatuses[t.Status],
		CreatedAt: timestamppb.New(t.CreatedAt),
		PostedAt:  optionalTime(t.PostedAt),
	}
	if t.ReversesID != nil {
		s := t.ReversesID.String()
		pb.ReversesId = &s
	}
	return pb
}

func holdPB(h store.Hold) *ledgerv1.Hold {
	return &ledgerv1.Hold{
		Id:             h.ID.String(),
		TransferId:     h.TransferID.String(),
		SourceId:       h.SourceID.String(),
		DestId:         h.DestID.String(),
		Amount:         h.Amount,
		CapturedAmount: h.CapturedAmount,
		ExpiresAt:      optionalTime(h.ExpiresAt),
		Status:         holdStatuses[h.Status],
	}
}

func optionalTime(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}
