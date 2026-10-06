// Package api serves the Supabase Management API subset (/v1, /v2, /platform) that
// Studio, the Supabase CLI and the Supabase MCP server call, over the registry and
// the lifecycle manager. Request and response types come from internal/api/gen,
// generated from the three public OpenAPI documents; every operation of those
// documents is routed, implemented or stubbed with a spec-derived valid response.
//
// See README.md for the route inventory, authentication, configuration and tests.
package api
