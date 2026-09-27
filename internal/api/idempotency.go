package api

import (
	"crypto/sha256"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

const idempotencyHeader = "Idempotency-Key"

// requestHash is SHA-256 over procedure, a zero byte, and the deterministic
// protobuf encoding of msg (A§7). The procedure is included because TopUp,
// Withdraw and Repay share one message shape.
func requestHash(procedure string, msg proto.Message) ([]byte, error) {
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(procedure))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil), nil
}

// idempotency reads the required Idempotency-Key header and hashes the request.
func idempotency[T any](req *connect.Request[T]) (key string, hash []byte, err error) {
	key = req.Header().Get(idempotencyHeader)
	if key == "" {
		return "", nil, ErrInvalidRequest("Idempotency-Key header is required.")
	}
	msg, ok := any(req.Msg).(proto.Message)
	if !ok {
		return "", nil, fmt.Errorf("request %T is not a proto message", req.Msg)
	}
	hash, err = requestHash(req.Spec().Procedure, msg)
	return key, hash, err
}
