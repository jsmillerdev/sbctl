# internal/domains

Per-project custom hostnames and vanity subdomains, the way hosted Supabase's Custom Domains work
(`supabase.com/docs/guides/platform/custom-domains`, and `/v1/projects/{ref}/custom-hostname` and
`/vanity-subdomain` of the Management API). The user guide is the deploy README's "Custom domains and
vanity subdomains"; this file is for people changing the code.

```go
svc := domains.New(domains.Options{Reg: reg, Config: cfg}) // Resolver defaults to net.DefaultResolver
st, err := svc.Initialize(ctx, ref, "api.acme.com")  // claim; returns the TXT token and CNAME target
st, err = svc.Reverify(ctx, ref)                      // DNS from this node; rate limited
st, changed, err := svc.Activate(ctx, ref)            // route + status 5; idempotent
wasActive, err := svc.Delete(ctx, ref)
```

The Management API (`internal/api/domains.go`) calls the service and then restarts the project's GoTrue
(`lifecycle.Reconfigurer.ApplyConfig`, Auth), which renders its external URL from the registry
(`domains.ExternalHost`, used by `lifecycle.presentedAuthURL`).

## State

`internal/registry/migrations/1190_custom_domains.sql` (range 1190-1219): `custom_hostnames` (one row per project:
hostname, status, TXT token, what the last DNS check found) and `vanity_subdomains`. They sit behind
`registry.DomainStore`, implemented by the Postgres and the memory registry with one conformance suite
(`internal/registry/domains_test.go`). The store writes the `routes` row itself, in the same transaction as the status
change, because routes are what the proxy serves and what its certificate gate allows: an activated hostname has a
`custom` route, a vanity subdomain a `vanity` route, a claim has none.

## Status values

The ones of hosted's API, which Studio's page switches on:

| Status | Meaning here |
|---|---|
| `2_initiated` | claimed; the TXT record is not found (`1_not_started` is never stored) |
| `3_challenge_verified` | the TXT record is there, the hostname does not reach this node |
| `4_origin_setup_completed` | both are there; ready to activate |
| `5_services_reconfigured` | active: routed, certificate requested, Auth presents it |

`Reverify` recomputes the status from DNS every time (a hostname whose DNS moved away goes back), except for an active
one, which it reports without a lookup.

## DNS checks

From the node's own resolver (`Options.Resolver`; tests pass a fake): the TXT record `_supavise-challenge.<hostname>` must
contain the claim's token (`supavise-verify=` and 48 hex digits, one per claim), and the hostname must be a CNAME of the
project's host `<ref>.api.<domain>` or resolve to an address the node answers on (the addresses of `api.<domain>` and of
the project's host, and `public_ip`). Studio's own pre-check of the CNAME, which asks Cloudflare's DNS-over-HTTPS from the
Studio process, is answered by the proxy from the same resolver (`internal/proxy/studio_cname.go`).

## Abuse limits

- A hostname is held by one project: a claim in status 4 or 5 holds it (a partial unique index in the registry, enforced
  again when a claim reaches 4), and `Initialize` refuses a hostname another project holds. Pending claims do not hold it, so
  nobody can lock a name they do not control; none can pass the TXT check without the DNS.
- Verification attempts: five per project in a burst, one more every 15 seconds, and fifty for the node in a burst with one
  more every 1.5 seconds (`Options.Attempts`, `Options.Refill`). A refused attempt makes no DNS query.
- Names: `ValidateHostname` refuses IP addresses, wildcards, single labels, bad labels, reserved zones and the node's own
  hosts; `ValidateVanityName` refuses reserved names and ref-shaped names (the proxy refuses the same two in a route row, so
  a vanity route cannot shadow a project even if written directly).
- A custom domain and a vanity subdomain are mutually exclusive, as on hosted.
- `tls.mode = "dns01"` nodes refuse custom hostnames (no per-host certificates there).

## Tests

```
go test ./internal/domains/     # names, the whole flow with a fake resolver, squatting, rate limits, vanity
```

The routing, the certificate gate and the Auth settings are tested in `internal/proxy` and `internal/lifecycle`; the
Pebble test (`internal/proxy/pebble-test.sh`, the `pebble` job of `linux.yml`) takes a hostname through the real flow
against an ACME test server and a DNS stub.
