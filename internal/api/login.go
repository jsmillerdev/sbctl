package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

// loginSessionTTL is how long a `supabase login` handshake stays claimable.
const loginSessionTTL = 10 * time.Minute

// maxLoginFailures is the wrong verification codes a session tolerates before it
// is destroyed (the code is 32 bits: this keeps guessing hopeless).
const maxLoginFailures = 5

func (s *Server) routesLogin(add func(string, handlerFunc)) {
	add("POST /platform/cli/login", s.cliLoginCreate)
	add("GET /platform/cli/login/{session_id}", s.cliLoginPoll)
	add("GET /platform/profile/access-tokens", s.listTokens)
	add("POST /platform/profile/access-tokens", s.createToken)
	add("GET /platform/profile/access-tokens/{id}", s.getToken)
	add("DELETE /platform/profile/access-tokens/{id}", s.deleteToken)
}

// newPAT creates and stores a personal access token for u and returns the token
// (shown once) and its row.
func (s *Server) newPAT(r *http.Request, u *User, name string, expires *time.Time) (string, *registry.AccessToken, error) {
	token := secrets.NewPAT()
	t := &registry.AccessToken{UserID: u.UserID, Name: name, Hash: secrets.HashToken(token), Prefix: token[:8], ExpiresAt: expires}
	if err := s.reg.CreateAccessToken(r.Context(), t); err != nil {
		return "", nil, err
	}
	return token, t, nil
}

func tokenJSON(m map[string]any, t *registry.AccessToken) map[string]any {
	setAll(m, map[string]any{
		"id": t.ID, "name": t.Name, "scope": "V0", "token_alias": t.Prefix + strings.Repeat("*", 8),
		"created_at": ts(t.CreatedAt), "last_used_at": nil, "expires_at": nil,
	})
	if t.LastUsedAt != nil {
		m["last_used_at"] = ts(*t.LastUsedAt)
	}
	if t.ExpiresAt != nil {
		m["expires_at"] = ts(*t.ExpiresAt)
	}
	return m
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) error {
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	ts, err := s.reg.ListAccessTokens(r.Context(), u.UserID)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(ts))
	for i := range ts {
		out = append(out, tokenJSON(elem("GET /platform/profile/access-tokens", ""), &ts[i]))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) error {
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	var in struct {
		Name      string  `json:"name"`
		ExpiresAt *string `json:"expires_at"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Name) == "" {
		return errf(http.StatusBadRequest, "name is required")
	}
	var exp *time.Time
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *in.ExpiresAt)
		if err != nil {
			return errf(http.StatusBadRequest, "expires_at must be an RFC 3339 timestamp")
		}
		exp = &t
	}
	token, t, err := s.newPAT(r, u, strings.TrimSpace(in.Name), exp)
	if err != nil {
		return err
	}
	m := tokenJSON(base("POST /platform/profile/access-tokens"), t)
	m["token"] = token
	writeJSON(w, http.StatusCreated, m)
	return nil
}

func (s *Server) tokenByPathID(r *http.Request) (*registry.AccessToken, *User, error) {
	u, err := s.currentUser(r)
	if err != nil {
		return nil, nil, err
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, nil, errf(http.StatusNotFound, "Access token not found")
	}
	ts, err := s.reg.ListAccessTokens(r.Context(), u.UserID)
	if err != nil {
		return nil, nil, err
	}
	for i := range ts {
		if ts[i].ID == id {
			return &ts[i], u, nil
		}
	}
	return nil, nil, errf(http.StatusNotFound, "Access token not found")
}

func (s *Server) getToken(w http.ResponseWriter, r *http.Request) error {
	t, _, err := s.tokenByPathID(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, tokenJSON(base("GET /platform/profile/access-tokens/{id}"), t))
	return nil
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) error {
	t, u, err := s.tokenByPathID(r)
	if err != nil {
		return err
	}
	if err := s.reg.DeleteAccessToken(r.Context(), u.UserID, t.ID); err != nil {
		return mapErr(err)
	}
	writeJSON(w, http.StatusOK, tokenJSON(base("DELETE /platform/profile/access-tokens/{id}"), t))
	return nil
}

// cliLoginCreate is the dashboard half of `supabase login`: the signed-in user
// authorizes the CLI's session. The server mints a token, seals it to the CLI's
// P-256 public key (ECDH, then AES-256-GCM keyed with the shared secret) and keeps
// the ciphertext until the CLI claims it with the 8-character code the dashboard
// shows (the nonce's first 8 hex digits). Protocol: supabase/cli
// command-internal/login-crypto.layer.ts and Studio pages/cli/login.tsx.
func (s *Server) cliLoginCreate(w http.ResponseWriter, r *http.Request) error {
	if s.cfg.API.DisableDeviceLogin {
		return errf(http.StatusForbidden, "Browser login is disabled; create an access token in the dashboard")
	}
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	var in struct {
		SessionID string `json:"session_id"`
		PublicKey string `json:"public_key"`
		TokenName string `json:"token_name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if !uuidRe.MatchString(in.SessionID) {
		return errf(http.StatusBadRequest, "session_id must be a uuid")
	}
	raw, err := hex.DecodeString(in.PublicKey)
	if err != nil {
		return errf(http.StatusBadRequest, "public_key must be hex")
	}
	clientPub, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return errf(http.StatusBadRequest, "public_key is not an uncompressed P-256 point")
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	shared, err := priv.ECDH(clientPub)
	if err != nil {
		return errf(http.StatusBadRequest, "invalid public_key")
	}
	block, err := aes.NewCipher(shared)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	name := strings.TrimSpace(in.TokenName)
	if name == "" {
		name = "cli_login"
	}
	token, t, err := s.newPAT(r, u, name, nil)
	if err != nil {
		return err
	}
	sealed := gcm.Seal(nil, nonce, []byte(token), nil) // ciphertext || 16-byte tag
	err = s.store.PutLoginSession(r.Context(), LoginSession{
		SessionID: in.SessionID, UserID: u.UserID, TokenID: t.ID, ServerPublicKey: hex.EncodeToString(priv.PublicKey().Bytes()),
		Nonce: hex.EncodeToString(nonce), Ciphertext: hex.EncodeToString(sealed), ExpiresAt: s.now().Add(loginSessionTTL),
	})
	if err != nil {
		_ = s.reg.DeleteAccessToken(r.Context(), u.UserID, t.ID)
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]string{"nonce": hex.EncodeToString(nonce)})
	return nil
}

// cliLoginPoll is the CLI half: it claims the sealed token with the verification
// code. The session works once; wrong codes burn it after maxLoginFailures.
func (s *Server) cliLoginPoll(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("session_id")
	code := r.URL.Query().Get("device_code")
	if !uuidRe.MatchString(id) || code == "" {
		return errf(http.StatusNotFound, "Login session not found")
	}
	sess, err := s.store.TakeLoginSession(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		return errf(http.StatusNotFound, "Cannot find login session")
	}
	if err != nil {
		return err
	}
	want := sess.Nonce[:8]
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(code)), []byte(want)) != 1 {
		s.loginMu.Lock()
		s.loginFails[id]++
		n := s.loginFails[id]
		if n >= maxLoginFailures {
			delete(s.loginFails, id)
		}
		s.loginMu.Unlock()
		if n < maxLoginFailures {
			if err := s.store.PutLoginSession(r.Context(), *sess); err != nil {
				return err
			}
		} else if sess.TokenID != 0 {
			_ = s.reg.DeleteAccessToken(r.Context(), sess.UserID, sess.TokenID)
		}
		return errf(http.StatusBadRequest, "Incorrect verification code")
	}
	s.loginMu.Lock()
	delete(s.loginFails, id)
	s.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"access_token": sess.Ciphertext, "public_key": sess.ServerPublicKey, "nonce": sess.Nonce,
	})
	return nil
}
