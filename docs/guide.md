# Using Supavise

This guide picks up after the install in the [README](../README.md#get-started). Your Supavise server is called a node. For server and AWS options in depth, see the [deploy guide](../deploy/README.md).

- [Claim the node and sign in](#claim-the-node-and-sign-in)
- [Create a project and connect an app](#create-a-project-and-connect-an-app)
- [Use the Supabase CLI](#use-the-supabase-cli)
- [Give an agent or a preview its own branch](#give-an-agent-or-a-preview-its-own-branch)
- [Back up and restore](#back-up-and-restore)
- [Monitor the node](#monitor-the-node)
- [Updates and maintenance](#updates-and-maintenance)
- [How Supavise compares](#how-supavise-compares)
- [Sizing and cost](#sizing-and-cost)
- [FAQ](#faq)

## Claim the node and sign in

The installer prints the dashboard URL and a one-time claim token, valid for 72 hours. On AWS, the stack outputs give you both. Open `https://api.<domain>/claim`, enter the token to create the first administrator, then sign in to the dashboard at `https://studio.<domain>`.

Invite your team from the organization's **Team** page, or on the server:

```bash
sudo -u supavise supavise users invite dev@example.com --role developer
```

## Create a project and connect an app

Create a project in the dashboard. Copy its publishable key from the project's API key settings, then connect with `supabase-js`:

```js
import { createClient } from '@supabase/supabase-js'

const supabase = createClient('https://<ref>.api.<domain>', '<publishable key>')
```

`<ref>` is the project ID, shown in the dashboard URL and in **Project Settings**. The URL is the only part that differs from supabase.com. Auth, REST, Realtime, Storage and Edge Functions all answer on it.

To serve a project on your own hostname or a short `<name>.api.<domain>` address, use **Project Settings**, **Custom Domains**, or `supabase domains` (see the deploy guide's [custom domains](../deploy/README.md#custom-domains-and-vanity-subdomains)).

## Use the Supabase CLI

The Supabase CLI reaches a node through a profile file. Print the profile on the node, then save the output as `supavise-profile.yaml` on your machine:

```bash
sudo -u supavise supavise api profile --format yaml
```

Create an access token in the dashboard, under **Account settings**, **Access Tokens**, then:

```bash
supabase --profile ./supavise-profile.yaml login --token sbp_...
supabase --profile ./supavise-profile.yaml link --project-ref <ref>
supabase --profile ./supavise-profile.yaml db push
supabase --profile ./supavise-profile.yaml functions deploy --use-api
```

## Give an agent or a preview its own branch

A branch is a full project with its own database, keys and URL. Create one with data copied from the parent:

```bash
supabase --profile ./supavise-profile.yaml branches create agent-task-42 --project-ref <ref> --with-data
supabase --profile ./supavise-profile.yaml branches get agent-task-42 --project-ref <ref>
```

`branches get` prints the branch's connection details. A branch with data needs the Owner or Administrator role; Developers create schema-only branches (leave out `--with-data`). A branch deletes itself after 7 days unless you mark it persistent.

The same branch is one call to `POST /v1/projects/<ref>/branches` with `{"branch_name": "agent-task-42", "with_data": true}`. The Supabase MCP server's branch tools work when you start it with `--api-url https://api.<domain>` and without `--project-ref`.

## Back up and restore

Backups start on their own. Every project archives its write-ahead log (WAL) continuously and takes a nightly base backup, and the same nightly run copies its Storage files and Edge Functions.

Backups go to local disk by default. To keep them off the server, pass `--s3-bucket` and `--s3-region` at install (see the deploy guide's [flags](../deploy/README.md#flags)). The AWS stack uses its own bucket.

**Restore from the dashboard.** Open **Database > Backups > Point in time**, pick a time within the retention window (7 days by default) and confirm. The project shows RESTORING, then returns to normal; anything written after that time is lost. Only Owners and Administrators can restore. The API equivalent is `POST /v1/projects/<ref>/database/backups/restore-pitr`.

**Restore on the server**, in place or as a copy under a new project:

```bash
sudo -u supavise supavise backups list <ref>
sudo -u supavise supavise backups restore <ref> --to 2026-10-06T14:30:00Z --as <newref>   # a copy at that time
sudo -u supavise supavise backups restore <ref> --to latest --force                      # in place
```

Good to know:

- Don't restart `supavise` while a project shows RESTORING.
- A restore needs free disk for a second copy of the project's data. The previous data stays on the server until the project's next successful restore.
- If a restore fails, the project shows RESTORE_FAILED and keeps its original data where possible. Restore again, or pause and resume the project. The [backup docs](../internal/backup/README.md) cover the details.

## Monitor the node

```bash
sudo -u supavise supavise status   # one verdict for the node and every project
curl https://api.<domain>/healthz  # for an uptime monitor; reveals nothing else
```

Add an `[alerts]` section to `/etc/supavise/config.toml` to get a webhook or email when a backup fails, disk runs low, a project turns unhealthy, a certificate nears expiry or an update is available. Before planned work, `supavise maintenance announce` shows a notice in the dashboard. The deploy guide's [health and alerts](../deploy/README.md#health-alerts-and-maintenance-notices) section has the details.

## How Supavise compares

| | Self-hosted Supabase (Docker Compose) | Supavise | Hosted Supabase |
|---|---|---|---|
| Projects | One per stack | Many per server, about 20 on an 8 GB server | Managed for you; each project gets its own Postgres instance |
| Organizations | One | Many, each with its own members and projects | Many |
| Dashboard | Studio for a single project | Supabase Studio in its hosted, multi-project mode | Supabase Studio |
| Management API and `supabase link` | Not available | Yes: the parts that Studio, the Supabase CLI and the MCP server call | Yes |
| Branching | Not available | Yes, schema-only or with data | Yes |
| Point-in-time restore | Not included; you set up backups yourself | Yes, in the dashboard (in place) or with `supavise backups restore` on the server | Yes |
| Edge Functions | Yes | Yes, on by default | Yes |
| Team roles and SSO | One shared dashboard login | Owner, Administrator, Developer and Read-only roles; SAML SSO | Yes |
| High availability | Not built in; you set it up yourself | No: one server, no failover or read replicas | Read replicas available |
| Where it runs | Your server | Your server or your AWS account | Supabase's cloud |
| Who you pay | Your hosting provider | Your hosting provider | Supabase |

Some hosted features depend on your Supabase plan. The self-hosted column follows [Supabase's self-hosting docs](https://supabase.com/docs/guides/self-hosting) and its Docker Compose setup. The docs also say that self-hosted Supabase is community-supported.

## Updates and maintenance

A Supavise release is a tested bundle: the `supavise` binary plus pinned versions of every Supabase service. Before a release ships, it passes the conformance suite and a test that upgrades a node with data from the previous release.

**The routine.** Run these on the server:

```bash
sudo supavise upgrade --check   # is there a newer release, and what changes?
sudo supavise upgrade --plan    # exactly what will update and restart
sudo supavise upgrade           # apply it
sudo -u supavise supavise status
```

`supavise upgrade` checks that the node is healthy and the release is signed, then backs up every project before it changes anything. It restarts Supavise (HTTPS pauses for a few seconds), updates the shared services one at a time, then updates projects: one canary first, then batches of five, with a health check after each. A project that restarts drops its connections briefly. The rollout stops at the first failure and puts the node back on the previous release.

**Going back.** `sudo supavise rollback` returns to the previous release; the node keeps the last three. If the newer release changed Supavise's own database, rollback refuses and explains how to restore that database from its pre-upgrade backup first.

**Automatic upgrades (opt-in).** By default the node only tells you a release exists. To let it upgrade itself inside a weekly window:

```bash
sudo supavise update config --mode auto --window "Sun 03:00-05:00"
sudo supavise update status    # settings, the next window, the last automatic run
```

An automatic upgrade runs only inside the window and only when the node is healthy and backed up. If one fails, automatic upgrades pause until you run `sudo supavise update resume`.

**Project upgrades.** As on hosted Supabase, a project's Owner or Administrator upgrades that project's services from **Project Settings**, **General**, **Service versions**, or with `sudo -u supavise supavise projects upgrade <ref>`. `sudo supavise upgrade --include-postgres` moves every project's Postgres release as part of a node upgrade.

**OS patches.** New installs apply security updates for Ubuntu or Debian automatically. When a patch needs a reboot, the node reboots inside the maintenance window, and projects start again on their own. Turn this off with `sudo supavise update config --os-security-updates=false` or `--os-reboot never`.

**Planned work.** `sudo -u supavise supavise maintenance announce --at "2026-10-12 22:00" --duration 2h --message "Database maintenance"` shows a notice in the dashboard and quiets alerts during the window.

## Sizing and cost

A node needs about 1.5 GB of memory for itself and about 150 MB per idle project. A project that serves traffic needs more, so size up for busy projects.

| Instance (AWS example) | Memory | Idle projects that fit |
|---|---|---|
| `t4g.medium` | 4 GiB | about 10 (a trial) |
| `t4g.large` (default) | 8 GiB | about 35, or about 20 with room for traffic |
| `t4g.xlarge` | 16 GiB | about 90 |
| `m7g.2xlarge` | 32 GiB | about 200 |

These figures come from measurements up to 50 projects on amd64 and arm64 ([docs/research/09-footprint.md](research/09-footprint.md)); the 32 GiB row extends them.

Each project has a compute size, Nano to 16XL as on hosted Supabase, Micro by default. Change it in Studio under Compute and Disk or with `supavise projects resize <ref> --size small`; `supavise projects sizes` shows which sizes the node can give now ([project sizes and disk](../deploy/README.md#project-sizes-and-disk)). On AWS you pay AWS directly for the instance, storage and traffic; the deploy guide lists each item in [What it costs](../deploy/README.md#what-it-costs).

## FAQ

**Is Supavise an official Supabase product?**
No. Supavise is an independent open-source project and is not affiliated with or endorsed by Supabase Inc. It runs Supabase's open-source services.

**Does it replace hosted Supabase?**
Not entirely. You get organizations, projects, the dashboard, the CLI, branching and backups on a server you run. The differences:

- One server, so no high availability, failover or read replicas. If the server stops, its projects stop.
- The dashboard restores a project in place. "Restore to new project" is missing: restore a copy with `supavise backups restore --as` on the server.
- Management API operations that Studio, the CLI and the MCP server don't call, such as billing and log drains, answer with empty placeholders.

**Do my apps need to change?**
No. Apps use the same client libraries and API paths as on supabase.com; only the host name changes, to `<ref>.api.<domain>`.

**Is my data backed up?**
Yes: databases, Storage files and Edge Functions, unless Storage uses its S3 backend (`[fleet] storage_backend = "s3"`), whose files stay in your own bucket: turn on versioning there. Every database archives its WAL continuously and takes a nightly base backup, and you can restore to any point in the retention window (7 days by default). The nightly run also copies each project's Storage files and Edge Function deployments to the same place, keeping only what changed.

- Backups go to local disk unless you give an S3 bucket. A bucket keeps them off the server.
- A restore returns files to the last nightly copy before the time you choose, not to that second. Hosted Supabase's database backups do not include Storage files, so this goes further than hosted.
- The master key, `/etc/supavise/master.key`, unseals the passwords inside backups and is not in them. Run `sudo -u supavise supavise system export-key` and keep the output offline, or keep an encrypted copy in the bucket with `system escrow-key`. Without the key, a lost server cannot be rebuilt from its backups.
- On AWS, daily snapshots of the data volume add a whole-node fallback.
