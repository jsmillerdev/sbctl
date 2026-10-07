<p align="center">
  <a href="https://supavise.dev">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="brand/readme/banner-dark.svg">
      <img alt="Supavise: many Supabase projects, one server" src="brand/readme/banner-light.svg" width="100%">
    </picture>
  </a>
</p>

<p align="center">
  Run many Supabase projects on one server, with the dashboard, API and tools of hosted Supabase.
</p>

<p align="center">
  <img alt="License: Apache-2.0" src="https://img.shields.io/badge/license-Apache--2.0-262626?labelColor=0b0b0b">
  <img alt="Runs as one Go binary" src="https://img.shields.io/badge/runs_as-one_Go_binary-262626?labelColor=0b0b0b">
  <img alt="Runs on Ubuntu 24.04+ or Debian 12+" src="https://img.shields.io/badge/runs_on-Ubuntu_24.04%2B_%7C_Debian_12%2B-262626?labelColor=0b0b0b">
  <img alt="amd64 and arm64" src="https://img.shields.io/badge/arch-amd64_%7C_arm64-262626?labelColor=0b0b0b">
</p>

<p align="center">
  <a href="https://supavise.dev">supavise.dev</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="deploy/README.md">Deploy guide</a> ·
  <a href="#documentation">Docs</a>
</p>

---

## What is Supavise?

Supavise turns one Linux server into a multi-project Supabase platform. You create and branch projects from the Supabase dashboard, the Supabase CLI or the Management API, and you back them up and restore them to a point in time with `supavise backups`.

Your apps connect with the same client libraries and API paths they use on supabase.com; only the host name changes. Supavise runs on an Ubuntu or Debian server of your own, or in your AWS account with one CloudFormation template.

<!-- screenshot:projects -->

## The gap it fills

Self-hosted Supabase runs one project per Docker Compose stack, with a single-project dashboard and no Management API. A second project means a second stack, and tools built for the hosted platform, such as `supabase link` and branching, have nothing to talk to.

Supavise keeps Supabase's own services and adds the platform layer around them, so one server can hold many projects and the hosted tools work against it.

| | Self-hosted Supabase (Docker Compose) | Supavise | Hosted Supabase |
|---|---|---|---|
| Projects | One per stack | Many per server, about 20 on an 8 GB server | Managed for you; each project gets its own Postgres instance |
| Dashboard | Studio for a single project | Supabase Studio in its hosted, multi-project mode | Supabase Studio |
| Management API and `supabase link` | Not available | Yes: the parts that Studio, the Supabase CLI and the MCP server call | Yes |
| Branching | Not available | Yes, schema-only or with data | Yes |
| Point-in-time restore | Not included; you set up backups yourself | Yes, with `supavise backups restore` on the server | Yes |
| Edge Functions | Yes | Yes, on by default | Yes |
| Team roles and SSO | One shared dashboard login | Owner, Administrator, Developer and Read-only roles; SAML SSO | Yes |
| Where it runs | Your server | Your server or your AWS account | Supabase's cloud |
| Who you pay | Your hosting provider | Your hosting provider | Supabase |

Some hosted features depend on your Supabase plan. The self-hosted column follows [Supabase's self-hosting docs](https://supabase.com/docs/guides/self-hosting), which also say that self-hosted Supabase is community-supported.

## What's included

The dashboard is the real Supabase Studio, built from an upstream tag with three small patches (hCaptcha, sign-in options and content security hosts) and running in its hosted, multi-project mode. Every other service is Supabase's own open-source release, unmodified.

<!-- screenshot:table-editor -->

**Projects**
- Create, pause, resume and delete projects from Studio, the Management API or the `supavise` CLI. A new project starts in about 3 seconds.
- Postgres with Supabase's full extension set, and the Supavisor pooler on ports 5432 and 6543.
- Auth, REST, GraphQL, Realtime, Storage and Edge Functions for every project.
- Publishable and secret API keys, legacy JWT keys, key rotation, and an option to turn legacy keys off.
- Auth, REST and pooler settings, edited in Studio and applied to the project.
- SAML single sign-on providers for each project's users.

**Branching for agents and previews**
- Branches through the Management API, the Supabase CLI and the Supabase MCP server.
- Schema-only branches that replay the parent's migrations and seed, or branches with data, cloned copy-on-write on XFS.
- A data branch's database has no outbound network access by default. Supavise replaces the parent's keys and passwords in the copied data and removes the parent's user sessions.
- A branch deletes itself after 7 days unless you mark it persistent.

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

## Quick start

Supavise needs a server running Ubuntu 24.04+ or Debian 12+ (amd64 or arm64) with ports 80, 443, 5432 and 6543 open. The [deploy guide](deploy/README.md) covers every option below in more detail.

### 1. Try it on any server

To try Supavise without a domain, run the installer as root and leave out `--domain`. The node then uses `<public ip>.sslip.io`, so you need no DNS setup.

```bash
curl -fsSL https://github.com/jsmillerdev/supavise/releases/latest/download/install.sh | sudo bash -s -- \
  --email you@example.com --firewall ufw
```

`--firewall ufw` turns on the host firewall with SSH and the four public ports open. If you manage the firewall yourself, use `--firewall none` instead.

With your own domain, first point its `api.`, `studio.`, `pooler.` and `*.api.` names at the server, then add `--domain example.com`. Add `--dns cloudflare --dns-credentials-file /root/cloudflare.env` (or `route53`, `hetzner`, `digitalocean`) for one wildcard certificate.

### 2. Or deploy to AWS

In the AWS console:

1. Download `supavise.yaml` from the [latest release](https://github.com/jsmillerdev/supavise/releases/latest).
2. Open CloudFormation, choose **Create stack**, then upload the file.
3. Enter a stack name and your email address. Leave the rest as it is.
4. Acknowledge that the stack creates IAM resources, then create the stack.
5. When the stack reaches `CREATE_COMPLETE`, open its **Outputs** tab. Run the `ClaimTokenCommand` value to read the claim token, and open `ClaimUrl`.

Or deploy from a terminal with the AWS CLI:

```bash
curl -fsSLO https://github.com/jsmillerdev/supavise/releases/latest/download/supavise-aws-deploy.sh
bash supavise-aws-deploy.sh --region us-east-1 --email you@example.com
```

The script waits for the stack, then prints the dashboard URL and the command that reads the claim token.

The stack creates one instance (Graviton `t4g.large` by default), an encrypted data volume with daily snapshots, and a versioned S3 bucket for backups. Deleting the stack keeps the bucket and a final snapshot. The [AWS section of the deploy guide](deploy/README.md#aws) covers your own domain, sizing and teardown.

### 3. Claim the node and sign in

The installer prints the dashboard URL and a one-time claim token, valid for 72 hours. On AWS, the stack outputs give you both. Open `https://api.<domain>/claim`, enter the token to create the first administrator, then sign in to the dashboard at `https://studio.<domain>`.

Invite your team from the organization's Team page, or on the server:

```bash
sudo -u supavise supavise users invite dev@example.com --role developer
```

### 4. Create a project and connect an app

Create a project in the dashboard. Then copy its publishable key from the project's API key settings and connect with `supabase-js`:

```js
import { createClient } from '@supabase/supabase-js'

const supabase = createClient('https://<ref>.api.<domain>', '<publishable key>')
```

The URL is the only part that differs from supabase.com. Auth, REST, Realtime, Storage and Edge Functions all answer on it.

### 5. Use the Supabase CLI

The Supabase CLI reaches a node through a profile file. Print the profile on the node, copy it to your machine, and create an access token in the dashboard:

```bash
sudo supavise api profile --format yaml > supavise-profile.yaml
```

```bash
supabase --profile ./supavise-profile.yaml login --token sbp_...
supabase --profile ./supavise-profile.yaml link --project-ref <ref>
supabase --profile ./supavise-profile.yaml db push
supabase --profile ./supavise-profile.yaml functions deploy --use-api
```

### 6. Give an agent or a preview its own branch

A branch is a full project with its own database, keys and URL. Create one with data copied from the parent:

```bash
supabase --profile ./supavise-profile.yaml branches create agent-task-42 --project-ref <ref> --with-data
supabase --profile ./supavise-profile.yaml branches get agent-task-42 --project-ref <ref>
```

`branches get` prints the branch's connection details. The same branch is one call to `POST /v1/projects/<ref>/branches` with `{"branch_name": "agent-task-42", "with_data": true}`, and the Supabase MCP server's branch tools work when you start it with `--api-url https://api.<domain>`.

### 7. Back up and restore

Backups start on their own: every project archives its WAL and takes a nightly base backup, on local disk or in S3 (`--s3-bucket` at install; the AWS stack uses its bucket). Restore on the server:

```bash
sudo -u supavise supavise backups list <ref>
sudo -u supavise supavise backups restore <ref> --to 2026-10-06T14:30:00Z --as <newref>   # a copy at that time
sudo -u supavise supavise backups restore <ref> --to latest --force                      # in place
```

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="brand/readme/architecture-dark.svg">
  <img alt="Supavise architecture: clients reach one node over HTTPS; the supavise binary fronts per-project Postgres, Auth and REST plus shared Supavisor, Realtime, Storage, Edge Runtime and Studio; backups go to S3 or local disk" src="brand/readme/architecture-light.svg" width="100%">
</picture>

Supavise is one Go binary, `supavise`. It installs Supabase's native service builds as systemd units and is itself the HTTPS proxy, the Management API (`/v1`, `/v2` and `/platform`), the project lifecycle engine and the backup service.

Each project gets its own Postgres cluster, Auth server and REST server. All projects share Supavisor, Realtime and Storage, which run in their multi-tenant mode as on the hosted platform, and they share one Studio and one Edge Runtime.

Every service runs in a systemd sandbox, and only the Supavise daemon can reach the cloud metadata service and its credentials. [DESIGN.md](DESIGN.md) explains the choices.

## Sizing and cost

A node needs about 1.5 GB of memory for itself and about 150 MB per idle project. A project that serves traffic needs more, so size up for busy projects.

| Instance (AWS example) | Memory | Idle projects that fit |
|---|---|---|
| `t4g.medium` | 4 GiB | about 10 (a trial) |
| `t4g.large` (default) | 8 GiB | about 35, or about 20 with room for traffic |
| `t4g.xlarge` | 16 GiB | about 90 |
| `m7g.2xlarge` | 32 GiB | about 200 |

These figures come from measurements up to 50 projects on amd64 and arm64 ([research/09-footprint.md](research/09-footprint.md)); the 32 GiB row extends them. On AWS you pay AWS directly for the instance, storage and traffic; the deploy guide lists each item in [What it costs](deploy/README.md#what-it-costs).

## FAQ

**Is Supavise an official Supabase product?**
No. Supavise is an independent open-source project and is not affiliated with or endorsed by Supabase Inc. It runs Supabase's open-source services.

**Does it replace hosted Supabase?**
Not entirely. You get projects, the dashboard, the CLI, branching and backups on a server you run. Supavise runs on one server, so it has no high availability, failover or read replicas; if the server stops, its projects stop. Backups and point-in-time restore run from the `supavise backups` command, not from the dashboard, and Management API operations beyond what Studio, the CLI and the MCP server call, such as billing and log drains, answer with empty placeholders.

**Do my apps need to change?**
No. Apps use the same client libraries and API paths as on supabase.com; only the host name changes, to `<ref>.api.<domain>`.

**Is my data backed up?**
Yes. Every database archives its WAL continuously and takes a nightly base backup, and you can restore to any point in the retention window (7 days by default). Backups go to local disk unless you give an S3 bucket, so use a bucket to survive the loss of the server. Database backups cover Postgres only. On AWS, daily snapshots of the data volume also cover Storage files, function bundles and the node's keys.

## Documentation

- [Deploy guide](deploy/README.md): install, DNS and TLS, AWS, backups, upgrades.
- [Design](DESIGN.md): architecture and decisions.
- [Management API](internal/api/README.md): what Supavise implements and how roles apply.
- [Branching](internal/branching/README.md): branch types, limits and the network filter.
- [Backups](internal/backup/README.md): WAL archiving, retention and restore.

## License

Apache-2.0. See [LICENSE](LICENSE).

Supavise is not affiliated with or endorsed by Supabase Inc. It runs Supabase's open-source services.
