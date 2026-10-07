// Package gen holds types generated from https://api.supabase.com/api/v1-json,
// v2-json and platform-json. Do not edit generated files by hand.
//
// The three specs are committed under specs/ and `go generate` never touches them,
// so generation and the server's route table are reproducible and offline-safe.
// Refresh the pins deliberately with `sh internal/api/gen/fetch-specs.sh` (what a
// `make specs` target should call), review the diff, then run `go generate`.
//
// Each spec generates into its own package because component names
// collide across them (v1 and v2 share many schemas).
//
// Types only: handlers live in internal/api and use these for every response.
package gen

//go:generate sh -c "d=$(mktemp -d) && for n in v1 v2 platform; do go run ./cmd/schemaonly specs/$n.json $d/$n.json && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config $n.cfg.yaml $d/$n.json || exit 1; done; rm -rf $d"
