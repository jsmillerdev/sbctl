# deploy

How a Supavise node gets installed, claimed, updated and released.

| Path | What |
|---|---|
| `install.sh` | The bootstrap for any Ubuntu 24.04+ or Debian 12+ server: host checks, release download with signature and checksum verification, then `supavise install`. |
| `release-assets.sh` | Signs a release (`SHA256SUMS`, `SHA256SUMS.sig`), stamps the public key into `install.sh`. Used by the release workflow and by the `install-e2e` job. |
| `cloudformation/supavise.yaml` | One-instance AWS stack: one required field, one file, nothing retained silently. |
| `aws/deploy.sh` | One-command AWS deploy (`aws cloudformation deploy`, `--dry-run`, `--delete`). Attached to releases as `supavise-aws-deploy.sh`. |
| `systemd/` | The unit templates the binary embeds (`systemd/README.md`). |

The work happens in `supavise install` (`cmd/supavise/cmd_install.go`), not in shell, so it has unit tests and one flag set. `install.sh` exists because the binary has to be fetched and verified before it can run.

## Install on a server

Create the DNS records first (below), then, as root:

```bash
curl -fsSL https://github.com/jsmillerdev/supavise/releases/latest/download/install.sh | sudo bash -s -- \
  --domain example.com --dns cloudflare --dns-credentials-file /root/cloudflare.env --email you@example.com
```

For a trial without a domain, leave `--domain` and `--dns` out: the node uses `<public ip>.sslip.io` and requests per-host certificates over HTTP-01.

`install.sh` checks the host (Ubuntu 24.04+ or Debian 12+, amd64 or arm64, glibc 2.35+, systemd; Ubuntu 22.04 is not supported because its polkit 0.105 ignores the JavaScript rule that lets the `supavise` user manage its units), downloads the release's `SHA256SUMS` and its signature, verifies the signature against the key stamped into the script, downloads `supavise-linux-<arch>`, checks its SHA-256 against the signed list, runs it with `--version` and refuses a binary that does not name the release tag (the signature covers the checksums, not the tag, so an older signed binary attached to a newer tag would otherwise be a downgrade), installs it at `/usr/local/bin/supavise` and runs `supavise install` with your flags. A signature or checksum that does not match stops the install before anything is changed. The copy of `install.sh` in the repository has no key; the one attached to a release does. `--binary PATH` installs a file you built instead (nothing verifies it).

`supavise install` then:

1. creates the `supavise` system user and the directories;
2. writes `/etc/supavise/config.toml` (mode 0600, owned by `supavise`) with the settings you passed that differ from the defaults;
3. installs the systemd units and the polkit rule (`supavise system install-units`), and installs polkit when the host lacks it;
4. opens TCP 80, 443, 5432 and 6543 in ufw when ufw is active (`--firewall ufw` installs, enables and configures it; the SSH ports come from what sshd or `ssh.socket` listens on, then `sshd -T` and `sshd_config.d`, then 22; `--firewall none` leaves the host alone);
5. creates the system project: `supavise system init` downloads the Postgres and auth artifacts, initializes the registry cluster and the dashboard GoTrue (sign-up disabled);
6. starts the shared services (`supavise fleet start`: postgres-meta, Supavisor, Realtime, Storage, and Studio when the release carries it);
7. enables and starts `supavise.service`, which starts the project units and the shared services again at every boot;
8. prints the dashboard URL and, while nobody has claimed yet, the claim token (or, with `--claim-token-file`, the file it is in).

A re-run that carries a new binary (the installer swaps the file, as a new release does) restarts `supavise.service` onto it; the daemon is compared with the installed file by content, so a re-run with the same release restarts nothing. Shared services and projects keep running.

Run it again at any time. A flag you leave out keeps its value in `config.toml`; the master key, the registry and the projects are never touched; a re-run that changes nothing restarts nothing. To change one setting, repeat the command with that flag (for example `--email`). `supavise install --print-config <flags>` shows the file a run would write without changing anything.

### Flags

Run `supavise install --help` for the full list. The ones most installs need:

| Flag | Meaning |
|---|---|
| `--domain example.com` | Base domain. The dashboard is `studio.<domain>`, the API `api.<domain>`, the pooler `pooler.<domain>`, a project `<ref>.api.<domain>`. |
| `--dns route53\|cloudflare\|hetzner\|digitalocean` | DNS-01 provider for a wildcard certificate. Credentials: `--dns-credentials-file` (`KEY=VALUE` lines, `api_token=...`) or `--dns-credential KEY=VALUE` (visible in the process list). On AWS the instance role is enough; add `--dns-credential hosted_zone_id=Z...` to skip the zone lookup. |
| `--email you@example.com` | ACME account contact. |
| `--s3-bucket B --s3-region R` | Keep WAL archives and base backups in S3. Without static keys the AWS credential chain applies (instance role). `--s3-endpoint`, `--s3-path-style`, `--s3-credentials-file` for S3-compatible stores. |
| `--key-passphrase-file F` | Keep an encrypted copy of the master key and `config.toml` in the backup backend, protected by the passphrase in `F` (mode 0600, at least 12 characters; nothing on the server stores it). Without it the summary reminds you to run `supavise system export-key` and keep the output offline. |
| `--public-ip` | Detected from the EC2 metadata service or `checkip.amazonaws.com` when omitted. |
| `--no-functions` | Run without Edge Functions. A new install turns them on; a re-run keeps the value in `config.toml`. |
| `--tls off` | Plain HTTP on 80 and 443, for tests or behind a TLS terminator. |
| `--set path=value` | Any `config.toml` setting, for example `--set ports.project_base=38000`. |
| `--claim-token-file PATH` | Write the token to a file (0600) and do not print it; the summary names the file. For unattended installs whose output is logged (the CloudFormation user data uses it). |

### DNS and TLS

With a domain, create these records, all pointing at the server (the installer prints them with your IP):

```
api.<domain>        studio.<domain>        pooler.<domain>        *.api.<domain>
```

The wildcard record for `*.api.<domain>` is required: Realtime and Storage resolve the project from the host name, so there is no path-based mode. With `--dns` and credentials, one wildcard certificate covers `*.api.<domain>`, `api.<domain>` and `studio.<domain>` (DNS-01). Without `--dns`, each host gets its own certificate on first request (HTTP-01), which needs every name to resolve to the server first. `internal/proxy/README.md` has the details. Running with TLS enabled accepts the certificate authority's subscriber agreement.

### Firewall and ports

Only 80, 443, 5432 and 6543 are meant to be reachable. The shared services listen on more interfaces than that (`internal/fleet/README.md`, "Supavisor's API and shard listeners"), so a host firewall or a security group that admits only those four ports is part of the install. With ufw installed but inactive (the default of several cloud images) and no `--firewall` flag, the installer stops and asks for `--firewall ufw` (it enables ufw with the SSH ports and the four public ports open) or `--firewall none` (you close the other ports yourself); it does not guess. With ufw not installed it prints a warning. The CloudFormation template does this with a security group.

## The first administrator and more users

`supavise-gotrue@system` has public sign-up disabled, so nobody can create a dashboard account through Studio. The first administrator is created with the claim token the installer printed:

1. Open `https://api.<domain>/claim`.
2. Enter the token, an email, a password (12 to 72 characters) and an optional organization name.
3. Sign in at `https://studio.<domain>`.

The token is single use and expires after 72 hours (`--claim-ttl`); the database keeps only its SHA-256. A request that fails (the address exists, the password is refused) gives the token back. Failed attempts are limited node-wide. While the node is unclaimed, `sudo -u supavise supavise claim token` issues a new token and revokes the old one (`--if-none` issues nothing while an unused, unexpired token exists, which is how a re-run of the installer avoids replacing a token that was already handed over, for example the one in the CloudFormation secret); after the claim it refuses unless you pass `--force` (an administrator locked out). The endpoint is `GET` and `POST /claim` on `api.<domain>` and on the loopback admin listener (`127.0.0.1:7000`).

Later users come by invitation, with a role as on hosted Supabase (owner, administrator, developer, read-only; see `internal/api/README.md` for what each may do). `supavise users invite` prints the link to give the invitee. Supavise sends no email unless `[mail]` is configured in `config.toml` (an SMTP relay for `supavise-gotrue@system`), in which case it also sends the invitation.

```bash
sudo -u supavise supavise users invite dev@example.com --role developer   # prints the sbi_... link (valid 7 days, works once)
sudo -u supavise supavise users invite ops@example.com --role administrator --org acme
sudo -u supavise supavise users invite qa@example.com --role read-only --project abcdefghijklmnopqrst
sudo -u supavise supavise users list                                       # who has which role
sudo -u supavise supavise users role dev@example.com administrator         # change a role (or restore an owner)
sudo -u supavise supavise users remove dev@example.com                     # ends their access at once (sessions and tokens are refused on the next request), removes memberships and the access tokens they made
```

An address that has no account gets the claim page with the token filled in: the invitee picks a password and joins with the invited role. An address that already has an account gets the dashboard's invitation page. The Team page of an organization in the dashboard does the same through the Management API. An organization always keeps one owner: `users remove` refuses to delete the only owner unless you add `--force`. Accounts created before roles existed became owners of every organization when the roles were introduced.

### Single sign-on

People can sign in to the dashboard with the company's identity provider (SAML 2.0: Okta, Entra ID, Google Workspace, ...). Register it with its metadata and the email domains it serves:

```bash
sudo -u supavise supavise sso add --metadata-url https://idp.example.com/saml/metadata --domain example.com --default-role developer
sudo -u supavise supavise sso list
sudo -u supavise supavise sso info          # the ACS URL and entity id to configure in the identity provider
```

`sso add` prints what to enter in the identity provider (the assertion consumer URL `https://api.<domain>/auth/v1/sso/saml/acs`, the entity id, the service provider metadata) and makes the dashboard's sign-in page offer "Continue with SSO". A person signs in with an address of one of the domains and, the first time, joins the organization with the default role. Anyone else who signs in through the provider is refused until an administrator approves them (`supavise sso pending`, `supavise sso approve <email> --role developer`, or `supavise sso deny`). A denied address, and one whose SSO account `supavise users remove` deleted, stays out: its next sign-in waits for approval and does not get the default role (`supavise sso allow <email>` lifts that). `supavise sso remove <id|domain>` removes the provider, ends its users' sessions and revokes their tokens. Owners and Administrators can do the same in the dashboard's organization settings. Projects have identity providers of their own for their end users: enable SAML in the project's Auth settings, then `supabase sso add --project-ref <ref>` with the profile of this node. `internal/api/README.md` ("Single sign-on") has the rules.

## Update

```bash
sudo supavise self-update            # the latest release
sudo supavise self-update --version v1.2.3
supavise self-update --check
```

`self-update` fetches the release, verifies the ed25519 signature of `SHA256SUMS` against the public key compiled into the binary (`internal/selfupdate/release_key.pem`) and the binary against its checksum, replaces `/usr/local/bin/supavise` with one rename (the previous binary stays as `supavise.prev`), refreshes the units with the new binary and restarts `supavise.service`. Then it waits up to `--wait` (5 minutes, as in the installer) for the daemon to answer on its admin listener, the check the installer uses (a daemon can be `active` to systemd and still crash a moment later). If it does not answer, `self-update` puts the previous binary back, re-renders the units with it and restarts the service. Project units keep running while the daemon restarts. Artifact versions move with `internal/versions/versions.yaml` inside a release, not through this command.

A binary built without a committed release key refuses to self-update (it names the missing key).

## AWS

One CloudFormation template (`cloudformation/supavise.yaml`) creates a complete node. You fill in an admin email; everything else has a default. There are three ways to deploy it, simplest first. All three give the same stack.

| You need | Path |
|---|---|
| A browser and an AWS account | [a. Console upload](#a-console-upload) |
| The AWS CLI | [b. `deploy.sh`](#b-deploysh) |
| Nothing but a click (after the maintainer's one-time setup) | [c. Launch Stack button](#c-launch-stack-button) |

Every release attaches two files for this: `supavise.yaml` (the template, with that release as its default) and `supavise-aws-deploy.sh` (the script of path b).

### a. Console upload

1. Download `supavise.yaml` from the [latest release](https://github.com/jsmillerdev/supavise/releases/latest).
2. In the AWS console, pick your region, open CloudFormation, then **Create stack, With new resources, Upload a template file**, and choose the file.
3. Enter a stack name (`supavise`) and your **Admin email**. Leave the rest as it is.
4. Tick **I acknowledge that AWS CloudFormation might create IAM resources**, then **Create stack**.
5. Wait about ten minutes for `CREATE_COMPLETE`, then open the **Outputs** tab (see [First login](#first-login)).

### b. deploy.sh

```bash
curl -fsSLO https://github.com/jsmillerdev/supavise/releases/latest/download/supavise-aws-deploy.sh
chmod +x supavise-aws-deploy.sh
./supavise-aws-deploy.sh --region us-east-1 --email you@example.com
```

With your own domain in Route 53:

```bash
./supavise-aws-deploy.sh --region us-east-1 --email you@example.com \
  --domain example.com --hosted-zone-id Z0123456789ABCDEFGHIJ
```

The script runs `aws cloudformation deploy`, waits, then prints the dashboard URL and the command that fetches the claim token. It needs the AWS CLI v2 and credentials that may create CloudFormation, EC2, IAM, S3, Route 53 and Secrets Manager resources. Other options: `--instance-type`, `--stack-name`, `--volume-size`, `--daily-snapshots`, `--version`, `--access-cidr`, `--ssh-cidr` with `--key-name`, `--no-session-manager`, `--ami-id`, `--data-snapshot-id`, `--profile`; `--help` lists them.

- `--dry-run` prints the exact `aws` commands, including the lookups it would make first (does the stack exist, which image does it run), and runs none.
- Running it again with the same `--stack-name` updates the stack. The script passes the image the instance already runs, so an update does not replace the instance when Canonical publishes a newer Ubuntu image.
- `--delete` stops the instance (when the stack has one), then deletes the stack, after a warning and a confirmation (see [Tear down](#tear-down)).

From a checkout, the script is `deploy/aws/deploy.sh` and finds the template by itself.

### c. Launch Stack button

CloudFormation reads a template from S3 only, so the button needs a public bucket that the maintainers own. Until they set one up, the release workflow skips this step and the releases carry no button. Setting it up takes the commands below, once, run by the repository owner with their own AWS credentials (nothing in this repository creates AWS resources). They create a bucket that serves only `templates/` to the public, an OIDC identity provider for GitHub, and a role that the release workflow can assume to write into `templates/`.

```bash
# 1. Choose a globally unique bucket name and its region.
BUCKET=supavise-templates-CHANGE-ME
REGION=us-east-1
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)

# 2. The bucket. Public access stays blocked except what the policy below allows.
aws s3api create-bucket --bucket "$BUCKET" --region "$REGION" \
  $([ "$REGION" = us-east-1 ] || echo --create-bucket-configuration LocationConstraint="$REGION")
aws s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=false,RestrictPublicBuckets=false

# 3. Public read of templates/ only (CloudFormation fetches the template without credentials).
aws s3api put-bucket-policy --bucket "$BUCKET" --policy '{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "PublicReadTemplates",
    "Effect": "Allow",
    "Principal": "*",
    "Action": "s3:GetObject",
    "Resource": "arn:aws:s3:::'"$BUCKET"'/templates/*"
  }]
}'

# 4. GitHub as an identity provider (once per account; skip when it exists:
#    aws iam list-open-id-connect-providers).
aws iam create-open-id-connect-provider \
  --url https://token.actions.githubusercontent.com --client-id-list sts.amazonaws.com

# 5. A role that only this repository's release workflow can assume, and that can only write templates/.
aws iam create-role --role-name supavise-release-templates --assume-role-policy-document '{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Federated": "arn:aws:iam::'"$ACCOUNT"':oidc-provider/token.actions.githubusercontent.com"},
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
        "token.actions.githubusercontent.com:sub": "repo:jsmillerdev/supavise:environment:release"
      },
      "StringLike": {
        "token.actions.githubusercontent.com:job_workflow_ref": "jsmillerdev/supavise/.github/workflows/release.yml@refs/tags/v*"
      }
    }
  }]
}'
aws iam put-role-policy --role-name supavise-release-templates --policy-name put-templates --policy-document '{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": "s3:PutObject",
    "Resource": "arn:aws:s3:::'"$BUCKET"'/templates/*"
  }]
}'

# 6. Tell the repository (variables, not secrets).
gh variable set AWS_TEMPLATE_BUCKET --repo jsmillerdev/supavise --body "$BUCKET"
gh variable set AWS_TEMPLATE_REGION --repo jsmillerdev/supavise --body "$REGION"
gh variable set AWS_RELEASE_ROLE_ARN --repo jsmillerdev/supavise --body "arn:aws:iam::$ACCOUNT:role/supavise-release-templates"
```

If step 2 fails with an access-denied error on the public access block, the account has Block Public Access switched on for all buckets; turn off only its "block public policy" and "restrict public buckets" settings (S3 console, Block Public Access settings for this account), or give up the button and use paths a and b.

The role trusts one token: the one the `release` job of `release.yml` gets for the GitHub Environment `release` on a `v*` tag of this repository (the `sub` and `job_workflow_ref` conditions). It cannot read, list or delete anything, and it cannot write outside `templates/`.

On the next release, the `publish-template` job uploads the release's template as `templates/supavise-<tag>.yaml` (and, for a stable tag, `templates/supavise-latest.yaml`), checks that an anonymous download returns the same bytes, and prints the Launch Stack link in the job log and the run summary. The link has this shape (URL-encode the template URL):

```
https://<region>.console.aws.amazon.com/cloudformation/home?region=<region>#/stacks/create/review?templateURL=<url-encoded https://BUCKET.s3.REGION.amazonaws.com/templates/supavise-latest.yaml>&stackName=supavise
```

Put that link in the README once it exists. The link creates the stack in the bucket's region; use paths a or b for another region.

### First login

The stack's **Outputs** list:

| Output | Meaning |
|---|---|
| `DashboardUrl` | Studio, for example `https://studio.example.com`. |
| `ClaimUrl` | The page that creates the first administrator. |
| `ClaimTokenCommand` | One `aws` command that prints the one-time claim token. |
| `ApiUrl` | The management API (Supabase CLI `--profile`, MCP `--api-url`). |
| `BackupBucket` | The S3 bucket with WAL archives and base backups. Kept when the stack is deleted. |
| `InstanceId`, `ConnectCommand` | The instance and the Session Manager command that opens a shell on it. |
| `DataVolumeId` | The EBS volume that holds the node's state. A final snapshot is kept when the stack is deleted. |
| `DataSnapshotsCommand` | One `aws` command that lists the daily snapshots of that volume (absent when `DailySnapshotsKept` is 0). |
| `PublicIp`, `DnsRecordsNeeded` | The Elastic IP and the DNS records you create yourself (see below). |

1. Run the `ClaimTokenCommand` value. It prints a token, `aws secretsmanager get-secret-value --region <region> --secret-id <ClaimTokenSecretArn> --query SecretString --output text`.
2. Open `ClaimUrl`, enter the token, an email, a password of 12 to 72 characters and an optional organization name.
3. Sign in at `DashboardUrl`.

The token works once and expires after 72 hours. To get a new one while nobody has claimed, open a shell (`ConnectCommand`) and run `sudo -u supavise supavise claim token`. [The first administrator and more users](#the-first-administrator-and-more-users) has the rest. The first HTTPS request to a new host name waits a few seconds while the node gets its certificate.

### Domain and DNS

- **Neither domain nor zone (the default).** The node uses `studio.<ip>.sslip.io`, `api.<ip>.sslip.io` and `<ref>.api.<ip>.sslip.io`, where `<ip>` is the Elastic IP. [sslip.io](https://sslip.io) is a free public service that resolves such names to the IP inside them, so no DNS work is needed. Each host gets its own Let's Encrypt certificate (HTTP-01) on first use, so port 80 must stay open. Use it for a trial: Let's Encrypt limits how many certificates one registered domain can get per week, and every project adds a host.
- **A domain in Route 53 (`DomainName` and `HostedZoneId`).** The stack creates the four DNS records (`api.`, `studio.`, `pooler.` and `*.api.` of the domain) and the node gets one wildcard certificate by DNS-01, using the instance role. The role can change only `_acme-challenge` TXT records of that zone. The zone must not already hold those four names, or the stack fails and rolls back.
- **A domain elsewhere (`DomainName` only).** Create the same four records at your DNS provider, all as `A` records pointing to the `PublicIp` output (`DnsRecordsNeeded` prints them). Create them early: each host's certificate is requested on its first visit and needs the name to resolve. The wildcard `*.api.<domain>` is required, because Realtime and Storage find the project from the host name.

### Sizing

A node needs about 1.5 GB of memory before the first project and about 150 MB for each idle project (measured, [docs/research/09-footprint.md](../docs/research/09-footprint.md)). The default instance, `t4g.large` (Graviton, 8 GiB), holds about 20 projects and leaves about 3.5 GiB for load. The table keeps 1 GiB for the operating system and counts idle projects only; a project that serves traffic needs more (memory per connection, cache, extensions), so size up for busy projects. The measurement reached 50 projects; the larger rows extend it.

| RAM | Instance types | Idle projects that fit |
|---|---|---|
| 4 GiB | `t4g.medium`, `t3.medium` | about 10 (a trial) |
| 8 GiB | `t4g.large` (default), `m7g.large`, `t3.large`, `m7i.large` | about 35; the default aims at about 20 |
| 16 GiB | `t4g.xlarge`, `m7g.xlarge`, `r7g.large`, `t3.xlarge`, `m7i.xlarge`, `r7i.large` | about 90 |
| 32 GiB | `m7g.2xlarge`, `r7g.xlarge`, `m7i.2xlarge`, `r7i.xlarge` | about 200 (beyond the measured 50) |

The `t` types are burstable: they run well while load is light and slow down when their CPU credits run out, so choose `m7g`/`m7i` (or larger) for sustained load. Graviton (`g`) types run the arm64 build and usually cost less; x86 types run the amd64 build. The template picks the image architecture from the instance type. The data volume (100 GiB by default) holds about 75 MB per new project plus what the projects store; raise `DataVolumeSize` for more. It can grow but never shrink: after the update, run `sudo xfs_growfs /var/lib/supavise` on the instance.

### What it costs

You pay AWS directly. Prices change by region and over time, so check the pages:

- The instance, billed per hour or second ([EC2 on-demand pricing](https://aws.amazon.com/ec2/pricing/on-demand/)). This is the largest part.
- Storage: the 30 GiB root volume and the data volume, plus the snapshots: the daily ones while the stack runs and the final one after a delete ([EBS pricing](https://aws.amazon.com/ebs/pricing/), snapshots section). A snapshot stores only the blocks that changed since the one before it, so the cost of keeping 7 grows with how much the volume changes; `DailySnapshotsKept` set to 0 takes none.
- The backup bucket: stored data, requests and versions kept for 30 days ([S3 pricing](https://aws.amazon.com/s3/pricing/)).
- Data transfer out of AWS, which API, database and Studio traffic produces ([EC2 on-demand pricing, data transfer](https://aws.amazon.com/ec2/pricing/on-demand/)).
- The public IPv4 address of the Elastic IP ([VPC pricing](https://aws.amazon.com/vpc/pricing/)).
- One Secrets Manager secret ([pricing](https://aws.amazon.com/secrets-manager/pricing/)), and with a hosted zone, Route 53 ([pricing](https://aws.amazon.com/route53/pricing/)).

### What the stack creates

One Ubuntu 24.04 instance (Graviton by default), an Elastic IP, a data volume formatted XFS and mounted at `/var/lib/supavise` (XFS with reflinks is what copy-on-write branching needs later), an S3 bucket for backups (versioned, encrypted, public access blocked, TLS only), a security group for ports 80, 443, 5432 and 6543, an instance role, a Secrets Manager secret for the claim token, and a Data Lifecycle Manager policy (with its own role) that snapshots the data volume every day. Without a VPC of your own it also creates a small VPC with one public subnet. Everything sits in one file: no nested stacks, no Lambda code, no custom resources, so the console can upload it as it is.

| Parameter | Meaning |
|---|---|
| `AdminEmail` | The only required field. Contact address for the Let's Encrypt account. |
| `InstanceType` | Default `t4g.large`. See [Sizing](#sizing). |
| `DataVolumeSize` | GiB for project data (default 100). |
| `DailySnapshotsKept` | Daily snapshots of the data volume to keep, a whole number from 0 to 1000 (default 7; `0` takes none and creates no policy). See [Backups and restore](#backups-and-restore). |
| `SupaviseVersion` | `latest`, or a release tag. The template of a release names that release. |
| `DomainName`, `HostedZoneId` | See [Domain and DNS](#domain-and-dns). |
| `AccessCidr` | Who may reach ports 80, 443, 5432 and 6543 (default everyone; port 80 must stay open for HTTP-01 without a hosted zone). |
| `EnableSessionManager` | `true` by default: lets you open a shell with `aws ssm start-session` and no inbound port. The role gets only the Session Manager channel actions; the AWS managed policy `AmazonSSMManagedInstanceCore` is not used because it can read every Parameter Store parameter in the account. |
| `SshCidr`, `KeyName` | Optional SSH (port 22) from that range, as the user `ubuntu` with that EC2 key pair. Off by default; Session Manager is the way in. |
| `VpcId`, `SubnetId` | An existing VPC and public subnet, both or neither. |
| `AmiId` | Empty on the first launch: the current Canonical Ubuntu 24.04 image. On later updates, give the image the instance runs (see below). |
| `DataSnapshotId` | Restore the data volume from the snapshot of an earlier stack (see [Tear down](#tear-down)). Set it only when creating a stack. |

The instance role may write only this bucket, put a value into only the claim-token secret, change only TXT records named `_acme-challenge.<domain>` or `_acme-challenge.*.<domain>` in the given hosted zone (`ChangeResourceRecordSets` with conditions on the record type and the normalized record name; `ListResourceRecordSets`, and `GetChange` on change IDs; the stack's own `DnsRecords` resource makes the A records), and, unless `EnableSessionManager` is `false`, use Session Manager. User data prepares the volume, runs `install.sh` from the release with the stack's parameters, stores the claim token in the secret, and signals the stack with `cfn-signal` (with a `curl` fallback when the helper package cannot be installed). The stack waits 30 minutes for that signal and fails with the installer's last error when it comes. The installer log is `/var/log/supavise-bootstrap.log` on the instance.

The role is what a process on the instance could take from the instance metadata service. The tenant-facing units cannot reach that service (`deploy/systemd/README.md`): every `supavise-*` unit denies it with `IPAddressDeny`, WAL archiving goes through the daemon (`internal/backup/README.md`, "The WAL relay"), and Storage's S3 backend needs a static key. Only `supavise.service` and the base backup units use the role.

If a create fails, CloudFormation rolls back and deletes what it made, except what is retained (see below): an empty backup bucket and a snapshot of the empty data volume may remain; delete them. To keep the instance for debugging, choose **Preserve successfully provisioned resources** under **Stack failure options** when you create the stack in the console.

### Node state and the data volume

The data volume holds everything the node needs to come back: the projects, the registry and the master key. User data bind-mounts `/etc/supavise` (the master key that unseals every secret in the registry, and `config.toml`) to `/var/lib/supavise/etc` before the installer runs, with an `fstab` entry and `RequiresMountsFor=` on the daemon and the system Postgres, so nothing the node needs to restart sits on the root volume. When the volume already holds an install, user data recreates the `supavise` user with the uid that owns the files and the installer runs as a repair that keeps the master key (an unchanged `config.toml` and registry are never touched; the claim token is kept too).

### Backups and restore

Every project archives its WAL to the backup bucket and takes a nightly base backup, and the same nightly run copies its Storage objects and Edge Function deployments to the bucket (only what changed; `<ref>/storage/` and `<ref>/functions/`). The daemon prunes old ones (`backup.retention_days`, 7 by default). Restore a project to a point in time on the instance (`aws ssm start-session`, then):

```bash
sudo -u supavise supavise backups list <ref>
sudo -u supavise supavise backups restore <ref> --to 2026-10-06T14:30:00Z --as <newref>   # a copy at that time
sudo -u supavise supavise backups restore <ref> --to latest --force                      # in place
```

`internal/backup/README.md` explains the options. The database comes back to the second you ask for. Storage objects and functions come back to the last nightly copy before that time: an object stored after the copy is missing and one deleted after it is back (hosted Supabase's database backups do not include Storage objects). Three limits matter on AWS:

- **The bucket alone cannot rebuild a node.** The passwords in a backup are sealed with the node's master key, which is not in the bucket in the clear. Keep it: `sudo -u supavise supavise system export-key` prints it for offline storage, and `system escrow-key --passphrase-file F` (or `--key-passphrase-file F` at install) stores a copy in the bucket encrypted with a passphrase only you know; `system restore-key` brings it back. Each key has its own copy in the bucket, so a rebuilt node never overwrites the old node's. Rebuild order: run `system restore-key` before `supavise install` (it needs a `config.toml` that names the bucket), so the installer keeps the old key; if the node already installed with a new key, `system restore-key --key-id <old key id> --force` puts the old one back before you restore any project. The daily snapshots below also cover the volume, until the volume is lost with the instance.
- **Deleting the stack keeps the bucket and takes a final snapshot of the volume** (see [Tear down](#tear-down)), so a deleted stack does not lose either.

#### Daily snapshots of the data volume

The bucket holds each project's database, Storage objects (the file backend on the data volume) and Edge Function deployments as of the last nightly run. It does not hold `config.toml` and the master key (unless you made the encrypted escrow above), and objects stored since the last nightly copy live on the data volume only. The stack therefore creates an Amazon Data Lifecycle Manager policy that snapshots the data volume once a day at 03:00 UTC and keeps the newest `DailySnapshotsKept` of them (7 by default; `0` creates neither the policy nor its role). The policy selects the volume by a tag whose value is the stack's ID, so it cannot reach another stack's volume. Every snapshot carries the volume's `Name` tag (`<stack name>-data`) and the tag `supavise:snapshot=daily`.

The policy runs under a role that only the Data Lifecycle Manager service can assume, for policies of this account and region. The role carries the AWS managed policy `AWSDataLifecycleManagerServiceRole`, which AWS documents as the permission set of the service's default role. It is wider than this one volume (it can create and delete snapshots in the account), and the template uses it deliberately: a hand-written subset that misses an action would stop the snapshots without a visible error. The instance cannot assume this role and holds no EC2 permission.

The snapshots are crash-consistent: they capture the volume at one moment, without what the instance still holds in memory, so they look like the disk after a power cut. Postgres replays its write-ahead log when it starts from one, which is the recovery a power cut needs, so the databases should come up. A file that a service was writing at that moment (a Storage upload, a function bundle being deployed) can be incomplete. That is why the S3 point-in-time backups stay the primary database backup, and the snapshots protect the rest of the volume (the master key, the configuration, objects newer than the nightly copy) and give a whole-node fallback. Prices: [EBS pricing](https://aws.amazon.com/ebs/pricing/) (see [What it costs](#what-it-costs)).

To restore from one, list the snapshots with the `DataSnapshotsCommand` output (or `aws ec2 describe-snapshots --owner-ids self --filters Name=tag:Name,Values=<stack name>-data`), pick one by its `StartTime`, and create a new stack with `DataSnapshotId` set to it (`--data-snapshot-id snap-...`), as in [Bring the node back](#tear-down). A volume made from a snapshot cannot replace the volume of a running stack, so use a new stack name while the old stack exists, or delete the old stack first. The whole node, projects included, returns to the state of that snapshot; to bring back one project to a point in time, use `supavise backups restore` above, which is finer.

### Update

On the instance, `sudo supavise self-update` (see [Update](#update)). That moves the binary; the stack keeps its parameters and the template it was created from. To change a parameter (for example `AccessCidr` or `DataVolumeSize`), update the stack: with `deploy.sh`, run it again with the new value (it keeps the instance's image); in the console, choose **Update, Use existing template**, and in **AmiId** enter the image ID the instance runs (EC2 console, instance details) if you left it empty at creation. An empty `AmiId` on an update may make CloudFormation look up the newest Ubuntu image again, and a changed image replaces the instance; this has not been checked in AWS, so give the ID.

### Replacing the instance

Do not change `AmiId`, `InstanceType` to another architecture, the subnet or the root volume of a running stack casually: CloudFormation replaces the instance, creates the new one first and then tries to attach the data volume to it while the old instance still holds it, so the update fails and rolls back. To move a node to a new Ubuntu image the supported path is an OS upgrade on the instance (`apt-get dist-upgrade`, then a reboot). Changing `InstanceType` within the same architecture restarts the instance in place, so it needs no special handling. When an instance really has to be replaced, swap it by hand around the update:

1. On the instance: `sudo systemctl stop supavise.service`, then `sudo -u supavise supavise fleet stop` and `sudo -u supavise supavise system stop` (the daemon leaves project units running; stopping them gives a clean data volume), then `sync`.
2. Take a snapshot of the data volume (the stack's `DataVolume` keeps a final snapshot when the stack or the volume is deleted, and the daily snapshots may be up to a day old, so take one now), and **detach the volume** (`aws ec2 detach-volume --volume-id <id>`; wait until it is `available`).
3. Update the stack with the new `AmiId`. The new instance attaches the volume, user data finds the earlier install and the node comes back with the same master key, registry and projects. The new instance has the same Elastic IP, so DNS stays.
4. When the update is done, check `sudo -u supavise supavise system status` and `supavise projects list`.

Not run in AWS: this procedure and the repair path in user data are untested; the first real replacement is their first test. Keep the snapshot from step 2 until it has worked once.

### Tear down

Deleting the stack never deletes your data silently:

| Resource | When the stack is deleted |
|---|---|
| Backup bucket (`BackupBucket`) | **Kept.** WAL archives and base backups stay, and so does the bucket policy. You pay for them until you delete the bucket. |
| Data volume (`DataVolumeId`) | **Snapshotted, then deleted.** The snapshot (encrypted) holds the master key, the registry and every project. You pay for it until you delete it. |
| Daily snapshots of the data volume | **Kept, and no longer pruned.** The stack deletes the lifecycle policy, and deleting a policy does not delete the snapshots it made. Delete the ones you do not need (`aws ec2 delete-snapshot`). |
| Instance, Elastic IP, security group, role, claim-token secret, DNS records, a VPC the stack made | Deleted. |

Stop the instance before you delete the stack. The final snapshot is taken from the volume when the stack deletes it, and a snapshot of a running node is only crash-consistent (see [Backups and restore](#backups-and-restore)). `./supavise-aws-deploy.sh --region <region> --stack-name supavise --delete` does it: it prints the bucket, volume and instance, asks you to type the stack name, stops the instance, waits until it is stopped, and only then deletes the stack (it keeps the stack if the instance does not stop). A stack whose first launch failed (`ROLLBACK_COMPLETE` or `CREATE_FAILED`) has no instance and no outputs: the script says so, stops nothing and deletes the stack; `cloudformation deploy` cannot update such a stack, so deleting it is the way to try again. In the console, stop the instance first (EC2, select the instance from the `InstanceId` output, **Instance state**, **Stop instance**; wait for **Stopped**), then in CloudFormation select the stack and choose **Delete**. Afterwards, find the snapshots, the final one and the daily ones:

```bash
aws ec2 describe-snapshots --region <region> --owner-ids self \
  --filters Name=volume-id,Values=<DataVolumeId> --query 'Snapshots[].[SnapshotId,StartTime,State]' --output text
```

**Bring the node back.** Create a new stack with `DataSnapshotId` set to that snapshot (`--data-snapshot-id snap-...`) and `DataVolumeSize` at least the snapshot's size. User data finds the earlier install on the volume and the installer runs as a repair that keeps the master key, the registry and the projects. The new stack makes a new bucket and a new Elastic IP, so update DNS if you manage it yourself; the new node writes new backups to the new bucket, and the old bucket still holds the older archives.

The existing administrator keeps working: the accounts are in the registry on the volume, so sign in at the new `DashboardUrl` as before. The repair issues no claim token when the volume's node is already claimed, so the new stack's claim-token secret does not hold one: user data replaces its placeholder with a note, and `ClaimTokenCommand` prints that note. When the volume's node was never claimed, user data leaves the secret as it is (the placeholder, or the token of an earlier boot of the same stack, which may still be valid), because the secret can hold the only copy of a live token. When you need a token (nobody can sign in, or the node was never claimed), open a shell (`ConnectCommand`) and run `sudo -u supavise supavise claim token --force` (without `--force` while nobody has claimed); it prints a token, which you enter at `ClaimUrl` as in [First login](#first-login).

**Delete everything.** After the stack is gone, delete the snapshot (`aws ec2 delete-snapshot`) and empty and delete the bucket (it is versioned: remove all versions, for example with the S3 console's **Empty** button) when you no longer need them.

### Checked and not checked

Checked offline, on every push (`.github/workflows/ci.yml`): `cfn-lint`; `checkov` (the six findings it reports are skipped in the template, each with a reason); `shellcheck` on `deploy.sh`; Go tests (`deploy/cloudformation`, `deploy/aws`) that parse the template and assert the parameters, the console form, the outputs, the retain policies, the IAM actions and their scoping, and run `deploy.sh` against a stub `aws` (argument errors, `--dry-run`, the create, update and delete paths, including stopping the instance before the delete); and `release-assets.sh`. **Not deployed:** nothing here has run in an AWS account. The first launch is the first test of the data-volume discovery, the restore from `DataSnapshotId`, the daily snapshot policy and its role (the first scheduled run is the first proof that the role carries the permissions the service needs), the `/snap/bin` fallback for the AWS CLI on Ubuntu 24.04 (and the `awscli` package), the Secrets Manager write, the signal, the Session Manager permissions (an SSM agent that needs more than the `ssm` and `ssmmessages` actions shows up as an instance that never appears in Session Manager), the dynamic lookup of the Ubuntu image, the Launch Stack link and the release job that uploads the template.

## Release signing

A release is signed by the maintainers' ed25519 key. Set it up once:

```bash
openssl genpkey -algorithm ed25519 -out supavise-signing.pem
openssl pkey -in supavise-signing.pem -pubout -out internal/selfupdate/release_key.pem   # commit this public file
gh secret set SUPAVISE_SIGNING_KEY --env release < supavise-signing.pem                     # then delete supavise-signing.pem
```

The key goes into a GitHub Environment named `release`, not into a repository secret. A repository secret can be read by any workflow run that someone with write access starts from any branch, and this key is the root of trust for self-update and `install.sh` on every node. Create the Environment in the repository settings (Settings, Environments) with a deployment rule that admits only the tags `v*`, and optionally a required reviewer; `release.yml` names it (`environment: release`) in the two jobs that read the key. The environment is created on the first run when it does not exist, but then it has no rule, so create it first.

`.github/workflows/release.yml` runs on a pushed tag `vMAJOR.MINOR.PATCH[-suffix]`. It stops early if the committed key is still the placeholder, if the secret (of the `release` environment) is missing or if the secret is not the private half of the committed public key. Then it runs `go vet` and `go test`, builds `supavise-linux-amd64` and `supavise-linux-arm64` (`CGO_ENABLED=0`, `-trimpath`, `-X main.version=<tag>`), builds Studio for both architectures with `studio/build.sh`, and `deploy/release-assets.sh` writes `SHA256SUMS`, signs it (`openssl pkeyutl -sign -rawin`, a raw 64-byte signature), stamps the public key into `install.sh`, copies `supavise.yaml` with the tag as its default `SupaviseVersion` and copies `aws/deploy.sh` as `supavise-aws-deploy.sh`. The job creates the release (a tag with a suffix becomes a pre-release) and attaches `supavise-linux-*`, the Studio archives, `SHA256SUMS`, `SHA256SUMS.sig`, `install.sh`, `supavise.yaml` and `supavise-aws-deploy.sh`. The template and the deploy script are not in `SHA256SUMS`: they are fetched over TLS from the release, like `install.sh`. When the repository variables `AWS_TEMPLATE_BUCKET` and `AWS_RELEASE_ROLE_ARN` exist, a last job uploads the template to the public bucket and prints the Launch Stack link ([AWS](#c-launch-stack-button)); without them it is skipped.

To rotate the key: generate a new pair, commit the new public file, replace the secret, tag a release. Binaries from before the rotation verify only against the old key, so they cannot self-update to a release signed with the new one; a rotation needs a manual reinstall with the new `install.sh`. Nothing has been tagged or released from this repository by the tooling.

## Tests

`tests/linux/install-e2e.sh` (the `install-e2e` job of `.github/workflows/linux.yml`, Ubuntu 24.04 on amd64 and arm64) runs the whole path on a fresh VM:

- release signing with a throwaway key; `install.sh --verify-only` accepts it and refuses a tampered binary, a changed checksum list, a bad signature, a release signed by another key and the keyless repository copy, each without installing anything;
- `install.sh --binary` with `--tls off` and `--public-ip 127.0.0.1` (every name is `*.127.0.0.1.sslip.io`, reached with Host headers): units active and enabled, file modes, loopback-only ports, the dashboard host through the proxy (with the slim Studio artifact of the pinned upstream version as a stand-in for our platform build), sign-up refused;
- a re-run of the installer before anyone has claimed keeps the claim token (no second token, none printed);
- the claim: wrong token refused, claim works once and fails the second time, sign-in, an invite redeemed and removed (and the removed user's access refused at once: unit tests, `internal/api/claim_test.go`);
- a personal access token, a project created through `POST /v1/projects`, its keys, a table created through `database/query`, a REST call through the proxy with the publishable key, Storage through the proxy, pooler logins on 5432 and 6543;
- a re-run changes nothing and restarts nothing, and a re-run with one flag changes that setting only;
- a re-run of `install.sh` with a v0.0.2 binary moves the daemon onto it (the daemon's `/proc/<pid>/exe` reports v0.0.2) without restarting shared services or projects;
- `supavise self-update` against a local release server: refuses a tampered binary, a wrong key and an older signed binary under a newer tag, installs v0.0.3, restarts the daemon, leaves the project's Postgres running; then a release whose daemon exits on `serve` is rolled back to v0.0.3, whose daemon answers again;
- the claim token stays out of the installer's output when `--claim-token-file` is used.

Go unit tests: `internal/selfupdate` (signature, checksum, atomic replace, refusals, an OpenSSL-made signature fixture), `internal/api/claim_test.go` (the endpoint, single use, expiry, rate limit, concurrent redemption, invites, user removal; the Postgres store runs when `SUPAVISE_TEST_DATABASE_URL` is set), `cmd/supavise/cmd_install_test.go` (flag to config mapping, minimal config rendering, `--set`, OS and glibc checks, EC2 metadata).

## Not done

- Uninstall: there is no `supavise uninstall`. Stop and disable `supavise.service` and the `supavise-*` units, then remove `/var/lib/supavise`, `/etc/supavise` and the `supavise` user by hand.
- `install.sh` resolves `latest` through a redirect of github.com and trusts TLS for that step only: the tag it gets is then used for signed files, so a wrong tag can only pick an older signed release.
- The claim page is at `api.<domain>/claim`, not `studio.<domain>/claim`: the Studio host belongs to Studio.
- No email: invites and the claim token are handed over out of band.
- The AWS stack is unverified in AWS (see "Checked and not checked" under AWS). The Launch Stack button needs the one-time bucket and role setup, which only the repository owner can do; until then, releases carry the template and `supavise-aws-deploy.sh` and the button is absent.
- The instance role of the AWS stack is reachable through IMDS from processes outside the `supavise-*` units (the daemon and the base backup units use it, as intended). A process that gets code execution as the `supavise` user inside a unit can still reach the daemon's and other units' memory through `/proc` (`deploy/systemd/README.md`, "What is not isolated"). Workstream J must keep `IPAddressDeny` on the edge runtime.
- Ubuntu 22.04 is out (polkit 0.105 ignores JavaScript rules). A sudoers drop-in for `systemctl start|stop|restart|enable|disable supavise-*` would bring it back at the cost of a `sudo` call in the supervisor; that needs a decision of the lead (HANDOFF section 0), so nothing is done.
