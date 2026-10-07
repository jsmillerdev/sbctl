# internal/sso

The building blocks of single sign-on (workstream L). The service that uses them, its routes and
the rules about who gets in are in `internal/api` (`sso_dashboard.go`, `sso_routes.go`, `sso_hook.go`;
the README there, "Single sign-on"); the commands are `supavise sso` (`cmd/supavise/cmd_sso.go`).

| File | What |
|---|---|
| `key.go` | `NewSigningKey`, `EnsureSigningKey`: the RSA 2048 key that `GOTRUE_SAML_PRIVATE_KEY` takes (PKCS#1 DER, standard Base64, no line breaks; GoTrue refuses anything weaker or with another exponent), created on first use and sealed in the registry as the project secret `saml_private_key`. One per project, the system project's for the dashboard. The creation races between the daemon and the CLI are settled by `registry.PutSecretIfAbsent`: the first writer wins. |
| `gotrue.go` | `Client`: the provider endpoints of one GoTrue's admin API (`/admin/sso/providers`, with its `service_role` key) and the provider objects in the shape of the Management API (GoTrue's own, without `disabled` and `resource_id`; `attribute_mapping.keys` and `domains` are always there, because the specs require them and GoTrue answers an empty mapping as `{}`). |
| `metadata.go` | domain syntax, `ValidateXML` (a SAML metadata document with an entity id and exactly one `IDPSSODescriptor`), `ResolveMetadata` (an https address is GoTrue's to fetch and refresh; a file is read and checked; a plain-http address is refused unless it is on this machine, where it is fetched here and passed as a document, because anyone on the path could otherwise replace the identity provider's signing certificate), `URLsFor` (the ACS URL and entity id an identity provider is configured with). |
| `hook.go` | the before-user-created hook of `supavise-gotrue@system`: `HookURL`, `HookSecret` (derived from the master key, `v1,whsec_...`), `VerifyWebhook` and `SignWebhook` (Standard Webhooks; a test pins the signature of the specification's own example), `HookEvent`. |

## Why GoTrue's own sign-up switch is not used

`GOTRUE_DISABLE_SIGNUP=true` also refuses the first sign-in of an SSO user (GoTrue's SAML endpoint creates the account with
the same code as sign-up, `createAccountFromExternalIdentity`, read at auth v2.195.0). So `supavise-gotrue@system` runs with sign-up
on and an HTTP before-user-created hook that the daemon answers (`api.Server.serveBeforeUserCreated`): a user of a
registered SAML provider, or an address an administrator invited by mail (a one-time grant in the invite), and nothing else.
GoTrue calls the hook on every creation path (`/signup`, `/otp`, `/magiclink`, `/invite`, anonymous sign-in, OAuth, SAML),
and fails the request when the hook cannot answer. The admin API's `POST /admin/users`, which the claim page uses, has no hook.

## Tests

`go test ./internal/sso` runs the unit tests (keys, signatures, metadata, the client against a stub). The behavior with a
real GoTrue and a real SAML identity provider is `internal/api/sso_integration_test.go` (development machines, needs
`pip install signxml` for the test identity provider) and `tests/linux/sso-smoke.sh` (CI, SimpleSAMLphp in a container).
