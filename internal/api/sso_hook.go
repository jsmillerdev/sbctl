package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/OWNER/sbctl/internal/sso"
)

// serveBeforeUserCreated answers sb-gotrue@system before it creates a user (its
// before-user-created hook, HTTP, signed with a secret derived from the master key). It allows
// exactly two kinds of new user:
//
//   - one whose account comes from a SAML identity provider that is registered here (the
//     provider id is in app_metadata.provider, which GoTrue sets, not the client);
//   - an address that an administrator invited by mail: the invite carries a one-time grant that
//     the daemon created for that address, and spending it is part of the answer.
//
// Everything else is refused, which is what keeps sign-up closed on a GoTrue whose
// GOTRUE_DISABLE_SIGNUP is off (it has to be: SSO users are created by signing up). The route is
// for GoTrue on the loopback interface only; a request that came through the edge proxy, or
// whose signature is wrong, is refused, and so is every request while the daemon cannot decide
// (GoTrue then fails the sign-up).
func (s *Server) serveBeforeUserCreated(w http.ResponseWriter, r *http.Request) error {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() ||
		r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return errf(http.StatusNotFound, "Not Found")
	}
	secret, err := sso.HookSecret(s.sec)
	if err != nil {
		return errf(http.StatusNotFound, "Not Found")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return errf(http.StatusBadRequest, "Bad Request")
	}
	if err := sso.VerifyWebhook(secret, r.Header, body, s.now()); err != nil {
		return errUnauthorized
	}
	var ev sso.HookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return errf(http.StatusBadRequest, "Bad Request")
	}
	msg, err := s.userCreationVerdict(r.Context(), &ev)
	if err != nil {
		return err
	}
	if msg != "" {
		s.log.Info("sign-up refused by the dashboard's sign-in hook", "reason", msg)
		writeJSON(w, http.StatusOK, sso.HookRefusal(msg))
		return nil
	}
	writeJSON(w, http.StatusOK, map[string]any{})
	return nil
}

// userCreationVerdict returns "" to let GoTrue create the user, or the reason it must not.
func (s *Server) userCreationVerdict(ctx context.Context, ev *sso.HookEvent) (string, error) {
	if id := ev.Provider(); id != "" {
		row, err := s.sso.provider(ctx, id)
		if err != nil {
			return "", err
		}
		if row == nil {
			return "This identity provider is not registered with sbctl.", nil
		}
		return "", nil
	}
	if tok := ev.GrantToken(); tok != "" {
		if p, _ := ev.User.AppMetadata["provider"].(string); p == "email" && ev.User.Email != "" {
			h := sha256.Sum256([]byte(tok))
			ok, err := s.accounts.Store.ConsumeSignupGrant(ctx, strings.ToLower(ev.User.Email), h[:], s.now())
			if err != nil {
				return "", err
			}
			if ok {
				return "", nil
			}
		}
	}
	return "Sign-up is closed on this dashboard. Ask an administrator for an invitation.", nil
}

// studioChanged re-renders Studio's unit in the background after the dashboard's SSO providers
// changed (Studio shows "Continue with SSO" only while there is one, and reads that when it
// starts). It is part of the operations a shutdown waits for.
func (s *Server) studioChanged(ctx context.Context) {
	if s.studioRefresh == nil {
		return
	}
	done, err := s.beginOp()
	if err != nil {
		return
	}
	go func() {
		defer done()
		s.studioMu.Lock()
		defer s.studioMu.Unlock()
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if err := s.studioRefresh(rctx); err != nil {
			s.log.Warn("Studio's sign-in page was not updated for the change of SSO providers; run `sbctl fleet start`", "error", err)
		}
	}()
}
