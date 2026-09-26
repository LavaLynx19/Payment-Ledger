package api

import (
	"context"
	"crypto/subtle"
	"strings"

	"connectrpc.com/connect"
)

// NewAuthInterceptor rejects every call whose bearer token isn't in tokens.
func NewAuthInterceptor(tokens []string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if !validToken(req.Header().Get("Authorization"), tokens) {
				return nil, ErrUnauthenticated()
			}
			return next(ctx, req)
		}
	}
}

// ParseTokens splits the comma-separated LEDGER_SERVICE_TOKENS value.
func ParseTokens(s string) []string {
	var tokens []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

func validToken(header string, tokens []string) bool {
	tok, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || tok == "" {
		return false
	}
	// Compare against every token so timing doesn't reveal which one matched.
	valid := false
	for _, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 {
			valid = true
		}
	}
	return valid
}
