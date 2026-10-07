<p align="center">
  <a href="https://supavise.dev">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="brand/lockups/supavise-horizontal.svg">
      <img alt="Supavise" src="brand/lockups/supavise-horizontal-black.svg" height="56">
    </picture>
  </a>
</p>

<p align="center">
  Run many Supabase projects on one server, with the dashboard, API and tools of hosted Supabase.
</p>

<p align="center">
  <a href="https://supavise.dev">supavise.dev</a> ·
  <a href="#install">Install</a> ·
  <a href="deploy/README.md">Deploy guide</a> ·
  <a href="DESIGN.md">Design</a>
</p>

---

Supavise turns one Linux server into a multi-project Supabase platform. You create and branch projects from the Supabase dashboard, the Supabase CLI or the Management API. Your apps connect with the same client libraries and API paths they use on supabase.com; only the host name changes.

## Why Supavise

Self-hosted Supabase runs one project per Docker Compose stack, with a single-project dashboard and no Management API. A second project means a second stack, and tools built for the hosted platform, such as `supabase link` and branching, have nothing to talk to.

Supavise fills that gap:

- **One server, many projects.** A node needs about 1.5 GB of memory for itself and about 150 MB per idle project. An 8 GB server holds about 20 projects with room for traffic.
- **The hosted experience.** Supabase Studio runs in its hosted, multi-project mode. The Supabase CLI links, pushes migrations and deploys functions as it does against supabase.com.
- **Environments for agents and teams.** A new project starts in about 3 seconds, so every developer, preview or AI agent can have its own database, keys and URL.

## Close to hosted Supabase

The goal is to behave like hosted Supabase, so nothing you build has to change. Supavise runs Supabase's own open-source service releases unmodified. The one exception is Studio, which carries three small patches for hCaptcha, sign-in options and content security hosts. Supavise also implements the parts of the Management API that Studio, the Supabase CLI and the Supabase MCP server call.

Conformance suites run the official `supabase-js` client and the Supabase CLI against a live node on amd64 and arm64. They run on every push to `main` and every night. Tests also check the API against Supabase's published OpenAPI specs, and a nightly job reports when those specs change.

## Features

**Projects**
- Create, pause, resume and delete projects from Studio, the Management API or the `supavise` CLI.
- Postgres with Supabase's full extension set, and the Supavisor pooler on ports 5432 and 6543.
- Auth, REST, GraphQL, Realtime, Storage and Edge Functions for every project.
- Publishable and secret API keys, legacy JWT keys, key rotation, and an option to turn legacy keys off.
- Auth, REST and pooler settings, edited in Studio and applied to the project.
- SAML single sign-on providers for each project's users.

**Branching**
- Branches through the Management API, the Supabase CLI and the MCP server.
- Schema-only branches that replay the parent's migrations and seed, or branches with data, cloned copy-on-write on XFS.
- A data branch's database has no outbound network access by default. Supavise replaces the parent's keys and passwords in the copied data and removes the parent's user sessions.

**Backups**
- Continuous archiving of the write-ahead log (WAL) and nightly base backups of every database, to S3 or local disk.
- Point-in-time restore with `supavise backups restore`, anywhere in the retention window, in place or as a new project.
- On AWS, daily snapshots of the data volume also cover Storage files and the node's keys.

**Teams**
- Owner, Administrator, Developer and Read-only roles, per organization or per project.
- Invitations, multi-factor authentication requirements, and SAML single sign-on for the dashboard with default roles and approval.

**Operations**
- Automatic HTTPS with Let's Encrypt, including wildcard certificates over DNS.
- Signed releases and `supavise self-update`, which verifies the signature before it replaces the binary.
- An AWS CloudFormation template that needs one field: your email address.

## How it works

Supavise is one Go binary, `supavise`. It installs Supabase's native service builds as systemd units and runs everything around them:

| Part | What it does |
|---|---|
| Edge proxy | Serves HTTPS and WebSockets, routes `<ref>.api.<domain>` to each project, and enforces API keys. |
| Management API | Implements the `/v1`, `/v2` and `/platform` APIs that Studio, the Supabase CLI and the MCP server call. |
| Lifecycle engine | Creates, pauses, resumes, branches and deletes projects. |
| Backup service | Archives WAL and base backups, and restores to a point in time. |

Each project gets its own Postgres cluster, Auth server and REST server. All projects share Supavisor, Realtime and Storage, which run in their multi-tenant mode as on the hosted platform, and they share one Studio and one Edge Runtime. Every service runs in a systemd sandbox, and only the Supavise daemon can reach the cloud metadata service and its credentials.

## Install

You need a server running Ubuntu 24.04+ or Debian 12+ (amd64 or arm64) with ports 80, 443, 5432 and 6543 open. If you use your own domain, first point its `api.`, `studio.`, `pooler.` and `*.api.` names at the server. Then run:

```bash
curl -fsSL https://github.com/jsmillerdev/supavise/releases/latest/download/install.sh | sudo bash -s -- \
  --domain example.com --dns cloudflare --dns-credentials-file /root/cloudflare.env --email you@example.com
```

To try Supavise without a domain, leave out `--domain` and `--dns`. The node then uses `<public ip>.sslip.io`.

The installer prints the dashboard URL and a one-time claim token, valid for 72 hours. Open `https://api.<domain>/claim`, enter the token to create the first administrator, then sign in to the dashboard.

### On AWS

Download `supavise.yaml` from the [latest release](https://github.com/jsmillerdev/supavise/releases/latest), create a stack from it in the CloudFormation console, and enter your email address. Or deploy from a terminal with the AWS CLI:

```bash
curl -fsSLO https://github.com/jsmillerdev/supavise/releases/latest/download/supavise-aws-deploy.sh
bash supavise-aws-deploy.sh --region us-east-1 --email you@example.com
```

The stack creates one instance (Graviton `t4g.large` by default), an encrypted data volume with daily snapshots, and a versioned S3 bucket for backups. The stack outputs give the claim URL and the command that reads the claim token. Deleting the stack keeps the bucket and a final snapshot. The [deploy guide](deploy/README.md) covers DNS, sizing, costs, upgrades and teardown.

### Use the Supabase CLI

The Supabase CLI reaches a node through a profile file. Print the profile on the node, copy it to your machine, and create an access token in the dashboard:

```bash
sudo supavise api profile --format yaml > supavise-profile.yaml
```

```bash
supabase --profile ./supavise-profile.yaml login --token sbp_...
supabase --profile ./supavise-profile.yaml link --project-ref <ref>
supabase --profile ./supavise-profile.yaml db push
```

## Documentation

- [Deploy guide](deploy/README.md): install, DNS and TLS, AWS, backups, upgrades.
- [Design](DESIGN.md): architecture and decisions.
- [Management API](internal/api/README.md): what Supavise implements and how roles apply.

## License

Apache-2.0. See [LICENSE](LICENSE).

Supavise is not affiliated with or endorsed by Supabase Inc. It runs Supabase's open-source services.
