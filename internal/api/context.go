package api

import "context"

type ctxKey int

const (
	ctxPrincipal ctxKey = iota
	ctxRequestID
)

// Principal is the authenticated caller.
type Principal struct {
	UserID string // GoTrue user uuid
	Email  string
	// Via is "jwt" (dashboard session) or "pat" (personal access token).
	Via string
	// TokenID is the access token row for Via == "pat".
	TokenID int64
}

func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxPrincipal, p)
}

// principalFrom returns the caller of a request that passed authentication.
func principalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxPrincipal).(*Principal)
	return p
}
