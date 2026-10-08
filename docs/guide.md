# Using Supavise

This guide picks up after the install in the [README](../README.md#get-started). Your Supavise server is called a node, and a second server that joins the first is another node. For server and AWS options in depth, see the [deploy guide](../deploy/README.md).

- [Claim the node and sign in](#claim-the-node-and-sign-in)
- [Create a project and connect an app](#create-a-project-and-connect-an-app)
- [Use the Supabase CLI](#use-the-supabase-cli)
- [Give an agent or a preview its own branch](#give-an-agent-or-a-preview-its-own-branch)
- [Back up and restore](#back-up-and-restore)
- [Monitor the node](#monitor-the-node)
- [Read replicas and failover](#read-replicas-and-failover)
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

Add an `[alerts]` section to `/etc/supavise/config.toml` to get a webhook or email when a backup fails, disk runs low, a project turns unhealthy, a certificate nears expiry, an update is available, or an upgrade starts, succeeds or fails. With a second server you also get one when a replica is unhealthy or lags, a server stops answering, a failover starts, ends or fails, or a server is fenced. Before planned work, `supavise maintenance announce` shows a notice in the dashboard. The deploy guide's [health and alerts](../deploy/README.md#health-alerts-and-maintenance-notices) section has the details.

## Read replicas and failover

A second Supavise server gives a project a read replica, a place to move the project to, and a standby of the registry that can take over. Both servers need an S3-compatible backup bucket, because the second server reads its first copy from it: pass `--s3-bucket` at install (see the deploy guide's [flags](../deploy/README.md#flags)). A replica always lives on a server other than the project's own. On AWS, make the second server with `supavise-aws-deploy.sh replica` ([Add a replica server](../deploy/README.md#add-a-replica-server)).

**Join a second server.** On the first server (the leader), create a one-time token. Save it in a file that only root can read on the new server, then install there with it:

```bash
sudo -u supavise supavise node token --region eu-west-1          # on the leader
curl -fsSL https://github.com/supavise/supavise/releases/latest/download/install.sh | sudo bash -s -- --join-token-file /root/join-token   # on the new server
sudo -u supavise supavise node ls                                 # on the leader: the new node is joining, then active
```

The servers talk over TCP port 7443, so open it between them. `--region` is the region that Studio shows for the new server's replicas.

**Add a replica.** In Studio, open **Project Settings**, **Infrastructure**, then **Add read replica**, and pick the new server's region. The button needs a project of size Small or larger. The same from the leader's shell, and for every project at once:

```bash
sudo -u supavise supavise replicas add <ref> --region eu-west-1
sudo -u supavise supavise replicas ls          # the setup step, status and lag
```

To give every project a replica on every other server, set `[replicas] default = "all"` in the leader's `/etc/supavise/config.toml`. That roughly doubles the footprint of the cluster.

**Use it.** A replica answers reads at `https://<identifier>.api.<domain>/rest/v1`, with the project's own keys, and through the pooler as the user `postgres.<identifier>`. Studio lists both next to the primary. `https://<ref>-lb.api.<domain>` is a load balancer that sends reads (`GET` and `HEAD` on `/rest/v1`) to the nearest healthy database and everything else to the primary. Point a latency-routed DNS record at each server to spread reads; a name that resolves to the primary's server always answers from the primary. [Read replicas reference](reference/replicas.md#names-and-ports) has the names.

**Switch over or fail over.** A switchover is planned and loses nothing. A failover follows a failure and loses at most what the replica had not received. Run each command with `--dry-run` first: it prints every precondition and changes nothing.

```bash
sudo -u supavise supavise projects failover <ref> --dry-run     # on the leader: one project
sudo -u supavise supavise failover --dry-run                    # on the server that should lead: the whole server
```

A project move needs a replica of that project. A server move needs one for every project, and Storage on S3 (`sudo -u supavise supavise storage migrate --to s3`). When the old primary is alive the move stops it cleanly and it follows as a replica, so moving back is the same command on the other server. When it is dead, the survivor fences it first. On AWS, `[failover] fencing = "aws"` stops the old instance and takes its Elastic IP. Elsewhere, set `fence_command` or pass `--old-primary-is-down`, which states that the old server cannot write. A server that returns after a failover starts no database until `sudo supavise node rejoin` rebuilds it.

By default every move is yours to start. `[failover] mode = "project"` or `"server"` lets the servers act on their own, on AWS only. The [reference](reference/replicas.md#failover) lists what a move does, the data a failure can cost and the failure matrix.

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
| Read replicas | Not available | Yes, per project on a second Supavise server, added in Studio or with `supavise replicas` | Yes |
| API load balancer | Not available | Yes, once a project has a replica: reads go to the nearest healthy database | Yes |
| Failover | Not built in; you set it up yourself | A switchover or failover of one project or the whole server, by hand; automatic on AWS only, with a fence | Managed by Supabase; depends on your plan |
| Storage on S3 | You set it up yourself | Optional, with `supavise storage migrate --to s3`; a server failover needs it | Yes |
| High availability | Not built in; you set it up yourself | Two servers: a standby copy of every replicated project and of the registry, so the second server can take over. One server alone has none. | Managed by Supabase; depends on your plan |
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

`supavise upgrade` checks that the node is healthy and the release is signed, then backs up every project before it changes anything. It also brings the host forward (unit files, directories, the firewall rule for the mesh port) with `supavise system converge`, which restarts no project. It restarts Supavise (HTTPS pauses for a few seconds), updates the shared services one at a time, then updates projects: one canary first, then batches of five, with a health check after each. A project that restarts drops its connections briefly. The rollout stops at the first failure and puts the node back on the previous release.

**Two servers and AWS stacks.** Run `supavise upgrade` on each server, the leader first when the release adds registry migrations: a follower one release behind runs against the leader's newer registry. While the two run different releases, automatic failover waits. On AWS, `sudo -E supavise upgrade --aws` also brings the CloudFormation stack forward, with your AWS credentials. It shows a change set and refuses any change that would replace the instance, the data volume or the address. [Bring a v0.1.x AWS stack forward](../deploy/README.md#bring-a-v01x-aws-stack-forward) has the steps, and the FAQ below the order to follow.

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

These figures come from measurements up to 50 projects on amd64 and arm64 ([docs/reference/footprint.md](reference/footprint.md)); the 32 GiB row extends them.

Each project has a compute size, Nano to 16XL as on hosted Supabase, Micro by default. Change it in Studio under Compute and Disk or with `supavise projects resize <ref> --size small`; `supavise projects sizes` shows which sizes the node can give now ([project sizes and disk](../deploy/README.md#project-sizes-and-disk)). On AWS you pay AWS directly for the instance, storage and traffic; the deploy guide lists each item in [What it costs](../deploy/README.md#what-it-costs).

## FAQ

**Is Supavise an official Supabase product?**
No. Supavise is an independent open-source project and is not affiliated with or endorsed by Supabase Inc. It runs Supabase's open-source services.

**Does it replace hosted Supabase?**
Not entirely. You get organizations, projects, the dashboard, the CLI, branching and backups on a server you run. The differences:

- High availability needs a second server. On one server, if the server stops, its projects stop. With two, you move a project or the whole server by hand, and on AWS the servers can do it on their own. [Read replicas and failover](#read-replicas-and-failover) has the steps.
- The dashboard restores a project in place. "Restore to new project" is missing: restore a copy with `supavise backups restore --as` on the server.
- Management API operations that Studio, the CLI and the MCP server don't call, such as billing and log drains, answer with empty placeholders.

**Do my apps need to change?**
No. Apps use the same client libraries and API paths as on supabase.com; only the host name changes, to `<ref>.api.<domain>`.

**Why do replicas need a second server?**
A replica on the project's own server would share the failures it should protect against (the disk, the host, the power) and would draw on the same memory and CPU, so Supavise does not offer one there: the setup answers "Read replicas on the same server as the primary are not offered." The second server also holds a standby copy of the registry, which lets it take over as leader. It can sit in another availability zone or region.

**What happens to Auth during a failover?**
A project's Auth (GoTrue) runs where the project's primary runs. A failover starts it again on the server that took over, behind the same project URL, with the same keys and JWT secret, so tokens that clients already hold stay valid. Its users and sessions live in the project's database, which the replica copied, so a failover loses the sign-ins and token refreshes that were inside the replication lag (the lag shows in Studio, and a failover refuses a replica more than 30 seconds behind unless you pass `--force`). A planned switchover loses nothing. While a move runs, the project shows RESTARTING and requests to it can fail; they work again at the same URL when the move ends.

**Can I use the file backend?**
It depends on which one:

- **Backups on local disk** (`[backup]` is a `file://` path). A second server cannot read them, so `supavise node token` refuses, and `[replicas] default = "all"` and the automatic failover modes are refused. Point `[backup]` at an S3-compatible bucket first.
- **Storage on files** (`[fleet] storage_backend = "file"`, the default). Replicas and the failover of one project work. A failover of the whole server refuses, because the files exist on the old server only. Move them with `sudo -u supavise supavise storage migrate --to s3`: it copies while Storage serves, then pauses Storage's writes for a short time (uploads get 503 with `Retry-After: 5`) while it switches, and reads fail while Storage restarts on the bucket. `--rollback` goes back.

**How do I upgrade an AWS stack made by v0.1.x?**
Follow the [deploy guide](../deploy/README.md#bring-a-v01x-aws-stack-forward) and keep this order. First rehearse on a throwaway stack: `deploy/aws/rehearse.sh` (in a checkout of the release's tag) makes one from the v0.1.1 template, updates it, checks that the instance was not replaced and deletes it; it costs money while it runs. Then on the node, run `sudo supavise upgrade`: the v0.1.x binary drives this first step and brings the binary and the host forward. After that, run `sudo -E supavise upgrade --aws` with your AWS credentials, or `supavise-aws-deploy.sh update --stack <name>` in CloudShell. Read the change set. It adds resources and permissions and changes tags; the script refuses anything that would replace the instance, the data volume or the address, and applies the rest only after you type `apply`. Nothing has run against a real stack in this repository's tests, so the rehearsal is the check.

**Is my data backed up?**
Yes: databases, Storage files and Edge Functions, unless Storage uses its S3 backend (`[fleet] storage_backend = "s3"`), whose files stay in your own bucket: turn on versioning there. Every database archives its WAL continuously and takes a nightly base backup, and you can restore to any point in the retention window (7 days by default). The nightly run also copies each project's Storage files and Edge Function deployments to the same place, keeping only what changed.

- Backups go to local disk unless you give an S3 bucket. A bucket keeps them off the server.
- A restore returns files to the last nightly copy before the time you choose, not to that second. Hosted Supabase's database backups do not include Storage files, so this goes further than hosted.
- The master key, `/etc/supavise/master.key`, unseals the passwords inside backups and is not in them. Run `sudo -u supavise supavise system export-key` and keep the output offline, or keep an encrypted copy in the bucket with `system escrow-key`. Without the key, a lost server cannot be rebuilt from its backups.
- On AWS, daily snapshots of the data volume add a whole-node fallback.
