package oauth_test

import (
	"testing"

	"github.com/supavise/supavise/internal/oauth"
	"github.com/supavise/supavise/internal/oauth/storetest"
)

// TestMemoryStoreSatisfiesTheStoreContract runs the contract tests that every oauth.Store must pass
// (internal/oauth/storetest, written beside the Postgres store) against the memory store.
func TestMemoryStoreSatisfiesTheStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) oauth.Store { return oauth.NewMemoryStore() })
}
