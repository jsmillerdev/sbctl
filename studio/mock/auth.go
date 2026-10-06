package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/OWNER/sbctl/internal/secrets"
)

// user is the dashboard user behind a verified access token.
type user struct {
	ID    string
	Email string
	Role  string
}

// authState verifies dashboard access tokens (HS256, GoTrue's JWT secret) and, when configured,
// plays a minimal GoTrue for the sign-in calls auth-js makes.
type authState struct {
	secret string
	b      *BuiltinAuth

	mu       sync.Mutex
	refresh  map[string]string // refresh token -> user id
	builtinU *user
}

func newAuthState(cfg *Config) *authState {
	a := &authState{secret: cfg.JWTSecret, b: cfg.BuiltinAuth, refresh: map[string]string{}}
	if cfg.BuiltinAuth != nil {
		sum := sha256.Sum256([]byte("mock-user:" + cfg.BuiltinAuth.Email))
		h := hex.EncodeToString(sum[:16])
		a.builtinU = &user{ID: h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], Email: cfg.BuiltinAuth.Email, Role: "authenticated"}
	}
	return a
}

// verifyRequest returns the user for a valid bearer token. The second result is "valid",
// "invalid" or "none" for the request log.
func (a *authState) verifyRequest(r *http.Request) (*user, string) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return nil, "none"
	}
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		tok, ok = strings.CutPrefix(h, "bearer ")
	}
	if !ok {
		return nil, "invalid"
	}
	claims, err := secrets.ParseHS256(strings.TrimSpace(tok), a.secret)
	if err != nil {
		return nil, "invalid"
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, "invalid"
	}
	if role, _ := claims["role"].(string); role != "authenticated" {
		return nil, "invalid"
	}
	email, _ := claims["email"].(string)
	return &user{ID: sub, Email: email, Role: "authenticated"}, "valid"
}

func (a *authState) accessToken(u *user, now time.Time) (string, int64) {
	exp := now.Add(time.Hour)
	claims := jwt.MapClaims{
		"iss": "mock-gotrue", "aud": "authenticated", "sub": u.ID, "email": u.Email, "role": "authenticated",
		"aal": "aal1", "amr": []map[string]any{{"method": "password", "timestamp": now.Unix()}},
		"session_id": u.ID, "is_anonymous": false, "iat": now.Unix(), "exp": exp.Unix(),
	}
	s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(a.secret))
	return s, exp.Unix()
}

func (a *authState) userJSON(u *user) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{
		"id": u.ID, "aud": "authenticated", "role": "authenticated", "email": u.Email,
		"email_confirmed_at": now, "phone": "", "confirmed_at": now, "last_sign_in_at": now,
		"app_metadata":  map[string]any{"provider": "email", "providers": []string{"email"}},
		"user_metadata": map[string]any{}, "identities": []map[string]any{{
			"identity_id": u.ID, "id": u.ID, "user_id": u.ID, "provider": "email",
			"identity_data": map[string]any{"email": u.Email, "sub": u.ID}, "created_at": now, "updated_at": now, "last_sign_in_at": now,
		}},
		"created_at": now, "updated_at": now, "is_anonymous": false,
	}
}

func (a *authState) session(u *user) map[string]any {
	now := time.Now()
	access, exp := a.accessToken(u, now)
	rt := secrets.RandomString(12, "abcdefghijklmnopqrstuvwxyz")
	a.mu.Lock()
	a.refresh[rt] = u.ID
	a.mu.Unlock()
	return map[string]any{
		"access_token": access, "token_type": "bearer", "expires_in": 3600, "expires_at": exp,
		"refresh_token": rt, "user": a.userJSON(u),
	}
}

// serve handles /auth/v1/* for the built-in token endpoint: password and refresh_token grants,
// user, logout. It is not GoTrue; it exists so the mock works without one.
func (a *authState) serve(w *respWriter, r *http.Request, body []byte) {
	path := strings.TrimPrefix(r.URL.Path, "/auth/v1")
	switch {
	case path == "/token" && r.Method == http.MethodPost:
		var in struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.Unmarshal(body, &in)
		switch r.URL.Query().Get("grant_type") {
		case "password":
			var p struct {
				Email    string `json:"email"`
				Password string `json:"password"`
			}
			_ = json.Unmarshal(body, &p)
			if p.Email != a.b.Email || p.Password != a.b.Password {
				w.json(http.StatusBadRequest, map[string]any{"code": 400, "error_code": "invalid_credentials", "msg": "Invalid login credentials"})
				return
			}
			w.json(http.StatusOK, a.session(a.builtinU))
		case "refresh_token":
			a.mu.Lock()
			_, ok := a.refresh[in.RefreshToken]
			delete(a.refresh, in.RefreshToken)
			a.mu.Unlock()
			if !ok {
				w.json(http.StatusBadRequest, map[string]any{"code": 400, "error_code": "refresh_token_not_found", "msg": "Invalid Refresh Token: Refresh Token Not Found"})
				return
			}
			w.json(http.StatusOK, a.session(a.builtinU))
		default:
			w.json(http.StatusBadRequest, map[string]any{"code": 400, "error_code": "validation_failed", "msg": "unsupported grant_type"})
		}
	case path == "/user" && r.Method == http.MethodGet:
		u, _ := a.verifyRequest(r)
		if u == nil {
			w.json(http.StatusUnauthorized, map[string]any{"code": 401, "error_code": "bad_jwt", "msg": "invalid JWT"})
			return
		}
		w.json(http.StatusOK, a.userJSON(u))
	case path == "/logout" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.json(http.StatusNotFound, map[string]any{"code": 404, "msg": "not found"})
	}
}
