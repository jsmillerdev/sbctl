package api

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/OWNER/sbctl/internal/lifecycle"
)

// Password limits: the Management API's own check is strength, which sbctl cannot judge;
// a minimum length and a bound that keeps the SCRAM computation cheap are enforced.
const (
	minDBPassword = 8
	maxDBPassword = 128
)

func (s *Server) routesDatabasePassword(add func(string, handlerFunc)) {
	add("PATCH /v1/projects/{ref}/database/password", s.updateDBPassword)
	add("PATCH /platform/projects/{ref}/db-password", s.updateDBPassword)
}

// updateDBPassword rotates the password of the project's postgres role: the cluster, the
// sealed secret the API itself connects with, and the pooler's cached logins. Existing
// sessions stay open; every service keeps working because it connects as a role of its own.
func (s *Server) updateDBPassword(w http.ResponseWriter, r *http.Request) error {
	p, err := s.running(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(in.Password); n < minDBPassword || n > maxDBPassword || strings.ContainsRune(in.Password, 0) {
		return errf(http.StatusBadRequest, "The password must be between %d and %d characters", minDBPassword, maxDBPassword)
	}
	rc, ok := s.mgr.(lifecycle.Reconfigurer)
	if !ok {
		return errf(http.StatusNotImplemented, "This server cannot change the database password")
	}
	done, err := s.beginOp()
	if err != nil {
		return err
	}
	defer done()
	ctx, cancel := detached(r, applyTimeout)
	defer cancel()
	if err := rc.SetDatabasePassword(ctx, p.Ref, in.Password); err != nil {
		if errors.Is(err, lifecycle.ErrInvalidState) {
			return errf(http.StatusConflict, "%v", err)
		}
		s.log.Error("database password change failed", "ref", p.Ref, "err", err)
		return errf(http.StatusInternalServerError, "Failed to update the database password")
	}
	resp := base("PATCH /v1/projects/{ref}/database/password")
	set(resp, "message", "Successfully updated database password")
	writeJSON(w, http.StatusOK, resp)
	return nil
}
