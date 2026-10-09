// Package storetest holds the contract tests that every oauth.Store implementation runs: the
// Postgres store (store_pg_test.go, when SUPAVISE_TEST_DATABASE_URL is set) and the memory store
// (store_mem_test.go). They are the definition of how a Store behaves; the Service relies on
// nothing else.
//
// This file is a stub: Run skips until the contract tests are written.
package storetest

import (
	"testing"

	"github.com/supavise/supavise/internal/oauth"
)

// Run runs the contract tests against stores that newStore makes. newStore returns a new, empty
// store on every call and cleans up with t.Cleanup; Run calls it once per test, so tests do not
// see each other's rows.
func Run(t *testing.T, newStore func(t *testing.T) oauth.Store) {
	t.Helper()
	t.Skip("the Store contract tests are not written yet")
}
