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

### Custom domains and vanity subdomains

A project can answer on its own hostname (`api.acme.com`) and on a short name under your domain (`acme.api.<domain>`), the way a hosted project does. Both work from the dashboard (**Project Settings**, **Custom Domains**), from the CLI (`supabase domains`, `supabase vanity-subdomains`) and from the Management API (`/v1/projects/{ref}/custom-hostname` and `/vanity-subdomain`). Owners and Administrators change them; every other role can read them. The project's own `<ref>.api.<domain>` keeps working, and the dashboard lists the Custom Domains add-on as included.

**Custom hostname.**

1. Create a CNAME record from the hostname to `<ref>.api.<domain>`. An A record to the server's address also works, but the dashboard asks for the CNAME. It checks that the name resolves from the server, not from the browser.
2. Add the hostname (`supabase domains create --custom-hostname api.acme.com`). The answer holds the TXT record to create: `_supavise-challenge.<hostname>` with a value that starts with `supavise-verify=`.
3. Verify (`supabase domains reverify`). The server looks up the TXT record and the hostname itself. The status moves from `2_initiated` to `3_challenge_verified` (TXT found, hostname not pointing at the server) and to `4_origin_setup_completed` (both found). It allows five attempts per project in a burst and one more every 15 seconds; past that it answers 429 with `Retry-After`.
4. Activate (`supabase domains activate`). The server checks the DNS records once more (this spends a verification attempt, and a record that has gone or moved sends the claim back to the status it now earns), routes the hostname, requests its certificate from the certificate authority at once (HTTP-01 or TLS-ALPN-01 on ports 80 and 443, which are already open), restarts the project's Auth and reports `5_services_reconfigured`. Delete (`supabase domains delete`) removes the route and the certificate, and Auth goes back to the project's own address.

The server asks the certificate authority only for hostnames that are active. A claimed or verified hostname gets no certificate, and neither does any other name that reaches port 443. One hostname belongs to one project: while a project has it verified or active, no other project can claim it. A claim alone does not lock the name, because nobody can verify a hostname without controlling its DNS. A verified claim that is neither activated nor verified again within 24 hours stops holding the name, so a former DNS controller cannot park it. A project has one custom hostname at a time. The hostname cannot be `api.<domain>`, `studio.<domain>`, `pooler.<domain>` or anything under `api.<domain>`.

Custom hostnames need per-host certificates, so they do not work with `--tls dns01` (only the wildcard is issued there; the API answers 400). With `--dns` and no `--tls dns01` (`auto`), the wildcard covers the project hosts and each custom hostname gets its own certificate. Every custom hostname must resolve to the server before it is activated, since the certificate authority connects to it.

**What changes in Auth.** As on hosted, activating a hostname makes Auth build its own address from it: `API_EXTERNAL_URL`, the confirmation, recovery, invite and email-change links, the OAuth redirect URIs (`https://<hostname>/auth/v1/callback`) and the SAML entity ID and endpoints. Add the new callback URL to each OAuth provider next to the old one, and update SAML identity providers that use the project's metadata. Nothing else changes: the token issuer (`iss`) stays `https://<ref>.api.<domain>/auth/v1`, so sessions and verifiers that pin it keep working when a domain is added or removed. Storage's S3 endpoint signs the hostname the client used, so S3 clients work on the custom hostname too (the fleet sets `S3_PROTOCOL_NON_CANONICAL_HOST_HEADER` and the proxy fills it).

**Vanity subdomain.** `<name>.api.<domain>` is covered by the `*.api.<domain>` DNS record (`supabase vanity-subdomains activate`). With `--dns` (`auto` or `dns01`) the wildcard certificate covers it and activation is immediate. Without a DNS provider, which includes every `sslip.io` node, each vanity name gets its own HTTP-01 certificate when it is activated, so the first request may wait for the certificate authority and every new name counts against its limits. A name is one DNS label of up to 63 letters, digits and hyphens, unique across the node. Names the node needs (`api`, `studio`, `pooler`, `www`, `admin`, `system`, and similar), names that look like a project ref (twenty lower-case letters) and names a project already holds are refused. It changes Auth the same way as a custom hostname does. A project uses one or the other, as on hosted: activating a vanity subdomain next to an active custom hostname, or the other way round, is refused with 409, and the vanity subdomain status then reads `custom-domain-used`.

`internal/domains/README.md` has the rules and `internal/proxy/README.md` the routing and certificate gate. CI covers the DNS checks, the refusals, the routing and the Auth settings in unit tests, and a customer hostname from initialize to activation, with REST and Auth fetched over the certificate Pebble issued, in the `pebble` job of `linux.yml`.

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

Owners delete an organization from its settings page in the dashboard, or on the server:

```bash
sudo -u supavise supavise orgs list
sudo -u supavise supavise orgs delete acme          # lists what it would delete and stops
sudo -u supavise supavise orgs delete acme --yes    # deletes its projects (each with a final backup), members, invitations and SSO setup
```

The node's last organization is never deleted.

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

### Project versions

A release pins the Postgres, GoTrue and PostgREST versions that new projects get. Existing projects keep the versions they run until their Owner or Administrator upgrades them, the way hosted Supabase lets a project owner decide when to upgrade: in Studio under Settings > General, "Service versions" (the "Upgrade project" button appears when the node pins newer versions than the project runs), with `POST /v1/projects/{ref}/upgrade`, or on the node:

```bash
supavise projects versions                 # what every project runs and whether an upgrade is available
supavise projects versions <ref>           # one project, with the last upgrade's outcome
supavise projects upgrade <ref>            # shows the plan, asks, upgrades one project
supavise projects upgrade --all --yes      # every eligible project: canary first, then batches; stops at the first failure
supavise artifacts gc --dry-run            # which unused artifacts would go
```

An upgrade takes a fresh base backup first (reason `pre-upgrade`) while the project serves, and changes nothing if the backup fails. Then GoTrue and PostgREST restart on the new releases (PostgreSQL too when its release changes) and every service is health checked; if one does not come up, the previous versions start again. The data directory stays where it is: these are minor-release changes of the same Postgres major version, so no data is copied and the project is offline for about a minute (longer when PostgreSQL restarts and recovers). Upgrades across Postgres major versions are refused, and so is a move to an older release than the project runs (after a rollback of the binary, the node pins older versions; a project already upgraded on the newer release stays on it, Studio offers no upgrade, and `supavise projects versions` lists it as ahead of the node). When the Postgres release changes, every extension installed in the project's databases is checked against it first (control file, library, update path), and an upgrade that the release cannot serve is refused with the extension named, before anything stops; afterwards the code of each extension must load, or the upgrade rolls back. The upgrade never runs `ALTER EXTENSION UPDATE`. `supavise projects upgrade` ignores a dropped SSH session (SIGHUP), and a project left `UPGRADING` by an upgrade whose process was killed is started again on its previous versions within a few minutes by the daemon (or when it starts), with nothing for the operator to do (an upgrade killed before it touched a service, during the backup, leaves the project running untouched). GoTrue's database migrations run when it starts and only go forward, so the pre-upgrade backup, not the rollback, is the way back for the data (`supavise backups restore`). The artifacts of the previous release stay on disk for `[upgrade] keep_releases` releases (default 3) and while any project runs them; `supavise projects upgrade` removes the rest after it succeeds. `[upgrade] canary_projects` (default 1) and `batch_size` (default 5) set the rollout of `--all`. Details: `internal/lifecycle/README.md`, "Service versions and project upgrades".

## Health, alerts and maintenance notices

```bash
sudo -u supavise supavise status           # one-line verdict, a table of components, the projects that need attention
sudo -u supavise supavise status --json    # the whole report
curl https://api.<domain>/healthz          # {"status":"healthy"}, for an uptime monitor
```

`supavise status` checks the daemon, the edge, the system cluster, each shared service and every project (Postgres answers, Auth and REST answer on loopback, Realtime and Storage hold the project's tenant), the age of each project's newest base backup, the free space on the state volume, the certificates, whether the backups hold an encrypted copy of the master key, and whether a newer release exists. The exit status is 0 for healthy, 1 for degraded (the node serves, something needs attention) and 2 for down (the daemon, the edge or the system cluster is not running). When `status` cannot check at all (an unreadable config, a user who may not read the node's files) it prints an error and exits 1 like any command, so a script that must tell "degraded" from "could not check" reads the `status` field of `--json` (no report, no verdict). A release that is available and a master key without a copy in the backups are notes: they do not change the verdict.

`GET /healthz` on the API host needs no credentials and answers `{"status":"healthy"}`, `{"status":"degraded"}` or `{"status":"down"}` and nothing else (no project names, no versions). It is a 200 unless the node is down, so a load balancer does not pull a node out of service because one project's PostgREST stopped; a monitor that matches `"healthy"` sees degraded. The daemon reuses a report for 20 seconds, so a monitor polling every few seconds costs one probe of the projects per 20 seconds. `GET /healthz/detail` returns the whole report to an Owner or Administrator (a dashboard session or a personal access token); the projects in it are those of the organizations the caller is an Owner or Administrator of.

The daemon sends alerts when something needs the operator, to webhooks and email:

```toml
[alerts]
email_to = "ops@example.com"                 # through [mail]
[[alerts.webhooks]]
url = "https://hooks.example.com/supavise"
secret = "a long random string"              # optional: signs timestamp and body (X-Supavise-Signature, X-Supavise-Timestamp)
```

It raises `disk_low`, `backup_failed` (a failed or stale backup), `project_unhealthy`, `certificate_expiring`, `node_unhealthy` (a shared service or the system cluster) and `update_available`. A problem is sent once, again as a reminder after 12 hours, and once more when it clears; it must last three minutes before it is sent, and no more than 20 notifications go out in an hour. While an upgrade runs or an announced maintenance window is open, the daemon does not raise or resolve `project_unhealthy` or non-critical `node_unhealthy` alerts, because the operator caused that downtime; a critical `node_unhealthy` (the system cluster or the registry down), `disk_low`, `backup_failed` and `certificate_expiring` are still sent. `sudo -u supavise supavise alerts test` sends a test alert to every destination and shows the result of each. Thresholds are in `[health]`, the rest in `[alerts]`; both are in `internal/health/README.md` and `internal/alerts/README.md`.

Once a day the daemon asks GitHub for the newest release, records it (`<state>/system/update.json`) and raises `update_available` once per version; it installs nothing. `[update] check_interval` changes the interval (24 hours by default, one hour at the least; `off` turns the check off).

`supavise maintenance announce --at "2026-10-12 22:00" --duration 2h --message "Database maintenance"` records a window that `supavise status` and `/healthz/detail` show while it is open, and that quiets the alerts described above; `supavise maintenance clear` removes it. A window longer than 24 hours needs `--allow-long`, and the alerts stop being quiet 24 hours after the start of a window that was not extended. An upgrade that is running (`<state>/system/upgrade.json`) is shown the same way; the marker counts only while it is under two hours old (by `started_at` or the file's modification time), so a crashed upgrade does not silence alerts for long. The dashboard banner stays empty for now: Studio draws any incident as "We are investigating a technical issue" with a link to Supabase's status page, and does not show the message. Tell dashboard users about planned downtime another way. An available update is never shown to dashboard users. `internal/notice/README.md` has the details.

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

One Ubuntu 24.04 instance (Graviton by default), an Elastic IP, a data volume formatted XFS and mounted at `/var/lib/supavise` (XFS with reflinks is what copy-on-write branching needs later), an S3 bucket for backups (versioned, encrypted, public access blocked, TLS only), a security group for ports 80, 443, 5432 and 6543, an instance role, a Secrets Manager secret for the claim token (and, when you give `KeyEscrowPassphrase`, one that carries the passphrase to the instance), and a Data Lifecycle Manager policy (with its own role) that snapshots the data volume every day. Without a VPC of your own it also creates a small VPC with one public subnet. Everything sits in one file: no nested stacks, no Lambda code, no custom resources, so the console can upload it as it is.

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
| `KeyEscrowPassphrase` | Optional, hidden (`NoEcho`), at least 12 characters. The installer stores a copy of the master key and `config.toml` in the backup bucket, encrypted with it (`--key-passphrase-file`), so the bucket can rebuild a node and an AWS install needs no manual `escrow-key`. Set it when you create the stack and keep it on later updates; empty means no copy (see [Backups and restore](#backups-and-restore)). |
| `DataSnapshotId` | Restore the data volume from the snapshot of an earlier stack (see [Tear down](#tear-down)). Set it only when creating a stack. |

The instance role may write only this bucket, put a value into only the claim-token secret (and, with `KeyEscrowPassphrase`, read and replace only the passphrase secret), change only TXT records named `_acme-challenge.<domain>` or `_acme-challenge.*.<domain>` in the given hosted zone (`ChangeResourceRecordSets` with conditions on the record type and the normalized record name; `ListResourceRecordSets`, and `GetChange` on change IDs; the stack's own `DnsRecords` resource makes the A records), and, unless `EnableSessionManager` is `false`, use Session Manager. User data prepares the volume, runs `install.sh` from the release with the stack's parameters, stores the claim token in the secret, and signals the stack with `cfn-signal` (with a `curl` fallback when the helper package cannot be installed). The stack waits 30 minutes for that signal and fails with the installer's last error when it comes. The installer log is `/var/log/supavise-bootstrap.log` on the instance.

The role is what a process on the instance could take from the instance metadata service. The tenant-facing units cannot reach that service (`deploy/systemd/README.md`): every `supavise-*` unit denies it with `IPAddressDeny`, WAL archiving goes through the daemon (`internal/backup/README.md`, "The WAL relay"), and Storage's S3 backend needs a static key. Only `supavise.service` and the base backup units use the role.

If a create fails, CloudFormation rolls back and deletes what it made, except what is retained (see below): an empty backup bucket and a snapshot of the empty data volume may remain; delete them. To keep the instance for debugging, choose **Preserve successfully provisioned resources** under **Stack failure options** when you create the stack in the console.

### Node state and the data volume

The data volume holds everything the node needs to come back: the projects, the registry and the master key. User data bind-mounts `/etc/supavise` (the master key that unseals every secret in the registry, and `config.toml`) to `/var/lib/supavise/etc` before the installer runs, with an `fstab` entry and `RequiresMountsFor=` on the daemon and the system Postgres, so nothing the node needs to restart sits on the root volume. When the volume already holds an install, user data recreates the `supavise` user with the uid that owns the files and the installer runs as a repair that keeps the master key (an unchanged `config.toml` and registry are never touched; the claim token is kept too).

### Backups and restore

Every project archives its WAL to the backup bucket and takes a nightly base backup, and the same nightly run copies its Storage objects and Edge Function deployments to the bucket (only what changed; `<ref>/storage/` and `<ref>/functions/`); the daemon prunes old ones (`backup.retention_days`, 7 by default). Owners and Administrators restore a project in place from the dashboard (Database > Backups > Point in time) or with the Management API (`POST /v1/projects/<ref>/database/backups/restore-pitr`). A restore also brings back the project's Storage objects and Edge Function deployments from the newest nightly copy at or before the target. The project shows RESTORING until it is back, or RESTORE_FAILED if the restore failed (restore again, pause and resume it, or delete it). A restore refuses to start when the disk lacks room for a second copy of the project's data, and the previous data directory stays next to the new one (`projects/<ref>/postgres/data.pre-restore-<time>`) until the project's next restore that works. Do not restart `supavise` while a project shows RESTORING: a restart that cuts the restore off leaves the project RESTORING until you settle it by hand (`internal/backup/README.md`, "From the dashboard and the Management API"). To restore a copy under a new project, or on any node without the dashboard, work on the instance (`aws ssm start-session`, then):

```bash
sudo -u supavise supavise backups list <ref>
sudo -u supavise supavise backups restore <ref> --to 2026-10-06T14:30:00Z --as <newref>   # a copy at that time
sudo -u supavise supavise backups restore <ref> --to latest --force                      # in place
```

`internal/backup/README.md` explains the options. The database comes back to the second you ask for. Storage objects and functions come back to the last nightly copy before that time: an object stored after the copy is missing and one deleted after it is back (hosted Supabase's database backups do not include Storage objects). Three limits matter on AWS:

- **The bucket alone cannot rebuild a node.** The passwords in a backup are sealed with the node's master key, which is not in the bucket in the clear. Keep it: `sudo -u supavise supavise system export-key` prints it for offline storage, and `system escrow-key --passphrase-file F` (or `--key-passphrase-file F` at install) stores a copy in the bucket encrypted with a passphrase only you know; `system restore-key` brings it back. On AWS, `KeyEscrowPassphrase` does this at the first boot: the stack puts the passphrase in a Secrets Manager secret (user data never holds it, and nothing prints it), the instance reads it into a root-only file for `--key-passphrase-file`, then overwrites the secret with a note and removes the file. The passphrase is in your account's Secrets Manager only for the minutes between stack creation and the install, and in the stack's parameters (`NoEcho` hides it in the console and in `describe-stacks`); keep your own offline copy, since neither the node nor the secret keeps it afterwards. An update that leaves the parameter empty removes the secret, and a changed value does not re-encrypt a copy that exists (run `system escrow-key` for that). `deploy.sh` has no option for it; use the console or `aws cloudformation create-stack`. Each key has its own copy in the bucket, so a rebuilt node never overwrites the old node's. Rebuild order: run `system restore-key` before `supavise install` (it needs a `config.toml` that names the bucket), so the installer keeps the old key; if the node already installed with a new key, `system restore-key --key-id <old key id> --force` puts the old one back before you restore any project. The daily snapshots below also cover the volume, until the volume is lost with the instance.
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

Checked offline, on every push (`.github/workflows/ci.yml`): `cfn-lint`; `checkov` (the six findings it reports are skipped in the template, each with a reason); `shellcheck` on `deploy.sh`; Go tests (`deploy/cloudformation`, `deploy/aws`) that parse the template and assert the parameters, the console form, the outputs, the retain policies, the IAM actions and their scoping, and run `deploy.sh` against a stub `aws` (argument errors, `--dry-run`, the create, update and delete paths, including stopping the instance before the delete); and `release-assets.sh`. **Not deployed:** nothing here has run in an AWS account. The first launch is the first test of the data-volume discovery, the restore from `DataSnapshotId`, the daily snapshot policy and its role (the first scheduled run is the first proof that the role carries the permissions the service needs), the `/snap/bin` fallback for the AWS CLI on Ubuntu 24.04 (and the `awscli` package), the Secrets Manager write, the read and replacement of the key escrow passphrase (and the IAM delay before the instance may read it, which the user data retries for a minute), the signal, the Session Manager permissions (an SSM agent that needs more than the `ssm` and `ssmmessages` actions shows up as an instance that never appears in Session Manager), the dynamic lookup of the Ubuntu image, the Launch Stack link and the release job that uploads the template.

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
- the claim token stays out of the installer's output when `--claim-token-file` is used;
- `supavise status --json` on the healthy node reports every check and exits 0; with the project's PostgREST stopped it exits 1, names the project and `/healthz` stays a 200 that says degraded; `/healthz/detail` needs a token; an announced maintenance window shows in `supavise status` and clears, while the Studio host's `/api/incident-banner` stays empty.

`tests/linux/upgrade-smoke.sh` (the `upgrade-smoke` job) covers project upgrades under systemd: projects on older GoTrue and PostgREST releases than the node's pins, kept on them by a node update, an upgrade onto a release that does not start (rolled back), the upgrade that works through the Management API (versions, the running processes, the data and an auth user intact), `supavise projects upgrade --all --yes` and `supavise artifacts gc` (`tests/linux/README.md`).

Go unit tests: `internal/selfupdate` (signature, checksum, atomic replace, refusals, an OpenSSL-made signature fixture), `internal/api/claim_test.go` (the endpoint, single use, expiry, rate limit, concurrent redemption, invites, user removal; the Postgres store runs when `SUPAVISE_TEST_DATABASE_URL` is set), `cmd/supavise/cmd_install_test.go` (flag to config mapping, minimal config rendering, `--set`, OS and glibc checks, EC2 metadata).

## Not done

- Custom hostnames: DNS-01 and HTTP-01 against a real certificate authority and real DNS have not been run (CI uses Pebble and a DNS stub). There is no wildcard custom hostname and no CAA check, and the limits on certificate orders are counted in memory per project and per server, not per organization.
- Uninstall: there is no `supavise uninstall`. Stop and disable `supavise.service` and the `supavise-*` units, then remove `/var/lib/supavise`, `/etc/supavise` and the `supavise` user by hand.
- `install.sh` resolves `latest` through a redirect of github.com and trusts TLS for that step only: the tag it gets is then used for signed files, so a wrong tag can only pick an older signed release.
- The claim page is at `api.<domain>/claim`, not `studio.<domain>/claim`: the Studio host belongs to Studio.
- No email: invites and the claim token are handed over out of band.
- The AWS stack is unverified in AWS (see "Checked and not checked" under AWS). The Launch Stack button needs the one-time bucket and role setup, which only the repository owner can do; until then, releases carry the template and `supavise-aws-deploy.sh` and the button is absent.
- The instance role of the AWS stack is reachable through IMDS from processes outside the `supavise-*` units (the daemon and the base backup units use it, as intended). A process that gets code execution as the `supavise` user inside a unit can still reach the daemon's and other units' memory through `/proc` (`deploy/systemd/README.md`, "What is not isolated"). Workstream J must keep `IPAddressDeny` on the edge runtime.
- Ubuntu 22.04 is out (polkit 0.105 ignores JavaScript rules). A sudoers drop-in for `systemctl start|stop|restart|enable|disable supavise-*` would bring it back at the cost of a `sudo` call in the supervisor; that needs a decision of the lead (HANDOFF section 0), so nothing is done.
