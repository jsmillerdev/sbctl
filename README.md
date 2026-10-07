<p align="center">
  <a href="https://github.com/jsmillerdev/supavise">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="brand/readme/banner-dark.svg">
      <img alt="Supavise: Supabase orgs and projects, self-hosted" src="brand/readme/banner-light.svg" width="100%">
    </picture>
  </a>
</p>

<p align="center">
  <b>Your own Supabase platform on one server.</b><br>
  Multiple organizations and projects, with the same dashboard, CLI and client libraries as hosted Supabase.
</p>

<p align="center">
  <a href="#get-started">Get started</a> ·
  <a href="docs/guide.md">Guide</a> ·
  <a href="deploy/README.md">Deploy guide</a> ·
  <a href="DESIGN.md">Design</a>
</p>

---

## Why Supavise

Self-hosted Supabase gives you one project per Docker stack and a single-project dashboard. Supavise turns one server into a full Supabase platform instead.

- **Many projects, one server.** Run about 20 projects with room for traffic on an 8 GB machine.
- **The tools you already use.** Supabase Studio, the Supabase CLI and `supabase-js` work as they do on supabase.com.
- **A database for every agent.** Give each preview or AI agent its own branch or project in seconds.

<!-- screenshot:projects -->

## Get started

> [!NOTE]
> The first release isn't published yet. The download links below work once it is.

**On your own server** (Ubuntu 24.04+ or Debian 12+):

```bash
curl -fsSL https://github.com/jsmillerdev/supavise/releases/latest/download/install.sh | sudo bash -s -- --email you@example.com
```

**On AWS:**

1. Download `supavise.yaml` from the [latest release](https://github.com/jsmillerdev/supavise/releases/latest).
2. In CloudFormation, create a stack from the file and enter your email address.
3. When the stack is ready, open its **Outputs** tab for the claim URL and token command.

Either way, open the claim URL, enter the token to create your admin account, and sign in. Then follow the [guide](docs/guide.md) to connect an app, use the CLI and create branches.

## What's included

| | |
|---|---|
| **Dashboard** | The real Supabase Studio, with multiple organizations and projects |
| **Every project** | Postgres, Auth, REST, GraphQL, Realtime, Storage and Edge Functions |
| **Branching** | Schema-only branches or full copies of your data, for previews and agents |
| **Backups** | Continuous backups with point-in-time restore |
| **Teams** | Roles, invitations, SAML single sign-on and MFA requirements |
| **API keys** | Publishable and secret keys, legacy JWT keys and key rotation |
| **Operations** | Automatic HTTPS, signed self-updates and a one-field AWS template |

<!-- screenshot:table-editor -->

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="brand/readme/architecture-dark.svg">
  <img alt="Supavise architecture: Supabase Studio, the Supabase CLI, the MCP server and your apps reach one node over HTTPS; the supavise binary fronts per-project Postgres, Auth and REST plus shared Supavisor, Realtime, Storage, Edge Runtime and Studio; backups go to S3 or local disk" src="brand/readme/architecture-light.svg" width="100%">
</picture>

Supavise is one Go binary that installs Supabase's own open-source services and runs the platform around them: HTTPS, the Management API, project lifecycle and backups. See [how Supavise compares](docs/guide.md#how-supavise-compares) to self-hosted and hosted Supabase.

## Learn more

- [Guide](docs/guide.md): connect an app, use the CLI, branches, backups, sizing and FAQ.
- [Deploy guide](deploy/README.md): domains and TLS, AWS options, upgrades and costs.
- [Design](DESIGN.md): architecture and decisions.

## License

[Apache-2.0](LICENSE). Supavise is not affiliated with or endorsed by Supabase Inc. It runs Supabase's open-source services.
