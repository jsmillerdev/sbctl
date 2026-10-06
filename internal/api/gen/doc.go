// Package gen holds types generated from https://api.supabase.com/api/v1-json,
// v2-json and platform-json. Do not edit generated files by hand.
//
// The three specs are committed under specs/ (refresh them with `make specs`,
// i.e. the curl lines below) so generation and the server's route table are
// reproducible. Each spec generates into its own package because component names
// collide across them (v1 and v2 share many schemas).
//
// Types only: handlers live in internal/api and use these for every response.
package gen

//go:generate sh -c "curl -fsSL https://api.supabase.com/api/v1-json -o specs/v1.json && curl -fsSL https://api.supabase.com/api/v2-json -o specs/v2.json && curl -fsSL https://api.supabase.com/api/platform-json -o specs/platform.json"
//go:generate sh -c "d=$(mktemp -d) && for n in v1 v2 platform; do go run ./cmd/schemaonly specs/$n.json $d/$n.json && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config $n.cfg.yaml $d/$n.json || exit 1; done; rm -rf $d"
