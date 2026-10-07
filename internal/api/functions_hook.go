package api

import (
	"context"
	"net/http"
)

// FunctionsHook is told when what a project's Edge Functions run has changed: after a
// function was created, deployed, patched or deleted, and after secrets were set or
// removed. The implementation (internal/functions) makes the files the runtime reads
// match the store; the API only stores, so a node without the runtime leaves Deps.Functions
// empty and nothing happens.
type FunctionsHook interface {
	// FunctionsChanged returns once the project's deployments and environment on disk
	// match the store. An error means the change is stored but not live yet.
	FunctionsChanged(ctx context.Context, ref string) error
}

// functionsChanged runs the hook, if any, and turns its failure into a 500 that says
// the change was stored: the deployment is not lost, the periodic reconcile of the hook
// retries it.
func (s *Server) functionsChanged(ctx context.Context, ref string) error {
	if s.fnHook == nil {
		return nil
	}
	if err := s.fnHook.FunctionsChanged(ctx, ref); err != nil {
		s.log.Error("edge functions: applying a stored change", "ref", ref, "err", err)
		return errf(http.StatusInternalServerError, "The change was stored but could not be applied to the Edge Functions runtime: %v", err)
	}
	return nil
}
