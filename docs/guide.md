# Using Supavise

This guide picks up after the install in the [README](../README.md#get-started). Your Supavise server is called a node. For server and AWS options in depth, see the [deploy guide](../deploy/README.md).

- [Claim the node and sign in](#claim-the-node-and-sign-in)
- [Create a project and connect an app](#create-a-project-and-connect-an-app)
- [Use the Supabase CLI](#use-the-supabase-cli)
- [Give an agent or a preview its own branch](#give-an-agent-or-a-preview-its-own-branch)
- [Back up and restore](#back-up-and-restore)
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

Backups start on their own. Every project archives its write-ahead log (WAL) continuously and takes a nightly base backup.

Backups go to local disk by default. To keep them in S3, pass `--s3-bucket` and `--s3-region` at install (see the deploy guide's [flags](../deploy/README.md#flags)). The AWS stack uses its own bucket.

Restore in the dashboard: open **Database > Backups**. The **Point in time** tab shows the span you can restore to, from the end of the oldest base backup up to now (7 days by default). Pick a time and confirm. The project shows RESTORING while it runs, goes offline for the restore, and returns to ACTIVE_HEALTHY when it finishes; everything written after the chosen time is lost. If the restore fails, the project shows RESTORE_FAILED (Studio shows "Something went wrong while restoring your project") instead of returning to normal. Supavise puts the original data back where it can, so the project's data is the data it had before the restore, or the project is down if that did not work; the Supavise log and the project's `restore.failed` event say why. Studio's failed screen offers only a delete. To move on, restore again (the Management API accepts a restore for a project in this state), pause and resume it with `supavise projects pause <ref>` and `supavise projects resume <ref>`, or delete it. Only Owners and Administrators can restore. The same restore is one call: `POST /v1/projects/<ref>/database/backups/restore-pitr` with `{"recovery_time_target_unix": <seconds>}`.

Before a restore starts, Supavise checks that the server's disk has room for a second copy of the project's data and refuses the restore with a 409 if it does not. Supavise keeps the project's previous data directory on the server, next to the new one (`projects/<ref>/postgres/data.pre-restore-<time>` in the state directory). Only the server's administrator can open or delete it; the project's next restore that works removes it once it has set aside a newer one, so at most one stays. A restore that fails leaves its attempt in `data.failed-restore-<time>` for the administrator to read, and the next restore that works removes it. After a restore, Supavise sets the database role passwords back to the ones it holds, so a database password you reset after the chosen time keeps working.

A restore runs inside the Supavise service. Do not restart `supavise` while a project shows RESTORING (the service waits about 11 minutes for work in flight, and a restore can take longer). A restart that cuts a restore off leaves the project RESTORING until the administrator settles it by hand: see "From the dashboard and the Management API" in `internal/backup/README.md`.

While point-in-time recovery is on (always, on Supavise), the **Scheduled backups** tab lists no backups, as on hosted projects with the add-on. To restore the state of one nightly base backup, use `POST /v1/projects/<ref>/database/backups/restore` with `{"id": <id>}` (`GET /v1/projects/<ref>/database/backups` lists the ids), or the command below.

Restore on the server, as a copy under a new project or in place:

```bash
sudo -u supavise supavise backups list <ref>
sudo -u supavise supavise backups restore <ref> --to 2026-10-06T14:30:00Z --as <newref>   # a copy at that time
sudo -u supavise supavise backups restore <ref> --to latest --force                      # in place
```

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

## Sizing and cost

A node needs about 1.5 GB of memory for itself and about 150 MB per idle project. A project that serves traffic needs more, so size up for busy projects.

| Instance (AWS example) | Memory | Idle projects that fit |
|---|---|---|
| `t4g.medium` | 4 GiB | about 10 (a trial) |
| `t4g.large` (default) | 8 GiB | about 35, or about 20 with room for traffic |
| `t4g.xlarge` | 16 GiB | about 90 |
| `m7g.2xlarge` | 32 GiB | about 200 |

These figures come from measurements up to 50 projects on amd64 and arm64 ([docs/research/09-footprint.md](research/09-footprint.md)); the 32 GiB row extends them. On AWS you pay AWS directly for the instance, storage and traffic; the deploy guide lists each item in [What it costs](../deploy/README.md#what-it-costs).

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
Your databases are. Every database archives its WAL continuously and takes a nightly base backup, and you can restore to any point in the retention window (7 days by default).

- Backups go to local disk unless you give an S3 bucket. A bucket keeps them off the server.
- Rebuilding a lost node also needs its master key, `/etc/supavise/master.key`, which is not in the bucket. On your own server, copy it and `/etc/supavise/config.toml` somewhere safe.
- Database backups cover Postgres only. On AWS, daily snapshots of the data volume also cover Storage files, function bundles and the master key.
