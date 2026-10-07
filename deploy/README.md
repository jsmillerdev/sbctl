# deploy

How an sbctl node gets installed, claimed, updated and released.

| Path | What |
|---|---|
| `install.sh` | The bootstrap for any Ubuntu 24.04+ or Debian 12+ server: host checks, release download with signature and checksum verification, then `sbctl install`. |
| `release-assets.sh` | Signs a release (`SHA256SUMS`, `SHA256SUMS.sig`), stamps the public key into `install.sh`. Used by the release workflow and by the `install-e2e` job. |
| `cloudformation/sbctl.yaml` | One-instance AWS stack. |
| `systemd/` | The unit templates the binary embeds (`systemd/README.md`). |

The work happens in `sbctl install` (`cmd/sbctl/cmd_install.go`), not in shell, so it has unit tests and one flag set. `install.sh` exists because the binary has to be fetched and verified before it can run.

## Install on a server

Create the DNS records first (below), then, as root:

```bash
curl -fsSL https://github.com/jsmillerdev/sbctl/releases/latest/download/install.sh | sudo bash -s -- \
  --domain example.com --dns cloudflare --dns-credentials-file /root/cloudflare.env --email you@example.com
```

For a trial without a domain, leave `--domain` and `--dns` out: the node uses `<public ip>.sslip.io` and requests per-host certificates over HTTP-01.

`install.sh` checks the host (Ubuntu 24.04+ or Debian 12+, amd64 or arm64, glibc 2.35+, systemd; Ubuntu 22.04 is not supported because its polkit 0.105 ignores the JavaScript rule that lets the `sbctl` user manage its units), downloads the release's `SHA256SUMS` and its signature, verifies the signature against the key stamped into the script, downloads `sbctl-linux-<arch>`, checks its SHA-256 against the signed list, runs it with `--version` and refuses a binary that does not name the release tag (the signature covers the checksums, not the tag, so an older signed binary attached to a newer tag would otherwise be a downgrade), installs it at `/usr/local/bin/sbctl` and runs `sbctl install` with your flags. A signature or checksum that does not match stops the install before anything is changed. The copy of `install.sh` in the repository has no key; the one attached to a release does. `--binary PATH` installs a file you built instead (nothing verifies it).

`sbctl install` then:

1. creates the `sbctl` system user and the directories;
2. writes `/etc/sbctl/config.toml` (mode 0600, owned by `sbctl`) with the settings you passed that differ from the defaults;
3. installs the systemd units and the polkit rule (`sbctl system install-units`), and installs polkit when the host lacks it;
4. opens TCP 80, 443, 5432 and 6543 in ufw when ufw is active (`--firewall ufw` installs, enables and configures it; the SSH ports come from what sshd or `ssh.socket` listens on, then `sshd -T` and `sshd_config.d`, then 22; `--firewall none` leaves the host alone);
5. creates the system project: `sbctl system init` downloads the Postgres and auth artifacts, initializes the registry cluster and the dashboard GoTrue (sign-up disabled);
6. starts the shared services (`sbctl fleet start`: postgres-meta, Supavisor, Realtime, Storage, and Studio when the release carries it);
7. enables and starts `sbctl.service`, which starts the project units and the shared services again at every boot;
8. prints the dashboard URL and, while nobody has claimed yet, the claim token (or, with `--claim-token-file`, the file it is in).

A re-run that carries a new binary (the installer swaps the file, as a new release does) restarts `sbctl.service` onto it; the daemon is compared with the installed file by content, so a re-run with the same release restarts nothing. Shared services and projects keep running.

Run it again at any time. A flag you leave out keeps its value in `config.toml`; the master key, the registry and the projects are never touched; a re-run that changes nothing restarts nothing. To change one setting, repeat the command with that flag (for example `--email`). `sbctl install --print-config <flags>` shows the file a run would write without changing anything.

### Flags

Run `sbctl install --help` for the full list. The ones most installs need:

| Flag | Meaning |
|---|---|
| `--domain example.com` | Base domain. The dashboard is `studio.<domain>`, the API `api.<domain>`, the pooler `pooler.<domain>`, a project `<ref>.api.<domain>`. |
| `--dns route53\|cloudflare\|hetzner\|digitalocean` | DNS-01 provider for a wildcard certificate. Credentials: `--dns-credentials-file` (`KEY=VALUE` lines, `api_token=...`) or `--dns-credential KEY=VALUE` (visible in the process list). On AWS the instance role is enough; add `--dns-credential hosted_zone_id=Z...` to skip the zone lookup. |
| `--email you@example.com` | ACME account contact. |
| `--s3-bucket B --s3-region R` | Keep WAL archives and base backups in S3. Without static keys the AWS credential chain applies (instance role). `--s3-endpoint`, `--s3-path-style`, `--s3-credentials-file` for S3-compatible stores. |
| `--public-ip` | Detected from the EC2 metadata service or `checkip.amazonaws.com` when omitted. |
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

Only 80, 443, 5432 and 6543 are meant to be reachable. The shared services listen on more interfaces than that (`internal/fleet/README.md`, "Supavisor's API and shard listeners"), so a host firewall or a security group that admits only those four ports is part of the install. With ufw inactive and `--firewall auto`, the installer prints a warning instead of guessing. The CloudFormation template does this with a security group.

## The first administrator and more users

`sb-gotrue@system` has public sign-up disabled, so nobody can create a dashboard account through Studio. The first administrator is created with the claim token the installer printed:

1. Open `https://api.<domain>/claim`.
2. Enter the token, an email, a password (12 to 72 characters) and an optional organization name.
3. Sign in at `https://studio.<domain>`.

The token is single use and expires after 72 hours (`--claim-ttl`); the database keeps only its SHA-256. A request that fails (the address exists, the password is refused) gives the token back. Failed attempts are limited node-wide. While the node is unclaimed, `sudo -u sbctl sbctl claim token` issues a new token and revokes the old one; after the claim it refuses unless you pass `--force` (an administrator locked out). The endpoint is `GET` and `POST /claim` on `api.<domain>` and on the loopback admin listener (`127.0.0.1:7000`).

Later users come by invitation. sbctl sends no email: it prints a token and you hand it over.

```bash
sudo -u sbctl sbctl users invite dev@example.com    # prints sbi_... (valid 7 days, works once)
sudo -u sbctl sbctl users list
sudo -u sbctl sbctl users remove dev@example.com    # also deletes the personal access tokens they made
```

The invitee opens the same claim page, enters the token and a password; the address comes from the invite. Every account is an administrator today (members and roles are a later phase), so invite only people you would give the whole node.

## Update

```bash
sudo sbctl self-update            # the latest release
sudo sbctl self-update --version v1.2.3
sbctl self-update --check
```

`self-update` fetches the release, verifies the ed25519 signature of `SHA256SUMS` against the public key compiled into the binary (`internal/selfupdate/release_key.pem`) and the binary against its checksum, replaces `/usr/local/bin/sbctl` with one rename (the previous binary stays as `sbctl.prev`), refreshes the units with the new binary and restarts `sbctl.service`. Then it waits up to `--wait` (2 minutes) for the daemon to answer on its admin listener, the check the installer uses (a daemon can be `active` to systemd and still crash a moment later). If it does not answer, `self-update` puts the previous binary back, re-renders the units with it and restarts the service. Project units keep running while the daemon restarts. Artifact versions move with `versions.yaml` inside a release, not through this command.

A binary built without a committed release key refuses to self-update (it names the missing key).

## AWS

[Launch the stack](https://console.aws.amazon.com/cloudformation/home#/stacks/quickcreate?templateURL=https%3A%2F%2Fsbctl-templates.s3.amazonaws.com%2Fsbctl.yaml&stackName=sbctl)

**This link does not work yet.** CloudFormation accepts templates from S3 only, and the maintainers have not published `sbctl.yaml` to a public bucket. To publish it, upload the `sbctl.yaml` asset of a release to a bucket that allows public reads of that object and replace the host in `templateURL` (the bucket name `sbctl-templates` above is a placeholder). Until then, create the stack from the file: console, Create stack, Upload a template file.

The stack creates one Ubuntu 24.04 instance (arm64 by default), an Elastic IP, a data volume formatted XFS and mounted at `/var/lib/sbctl` (XFS with reflinks is what copy-on-write branching needs later), an S3 bucket for backups (versioned, encrypted, public access blocked, kept when the stack is deleted), a security group for 80, 443, 5432 and 6543, an instance role, and a Secrets Manager secret for the claim token. If you give no VPC it also creates a small VPC with one public subnet.

| Parameter | Meaning |
|---|---|
| `AdminEmail` | ACME contact (required). |
| `Architecture`, `InstanceType` | arm64 with `t4g.medium` by default. About 100 MB of RAM per idle project: 4 GiB fits a handful of small projects, 16 GiB about a hundred. |
| `DataVolumeSize` | GiB for project data (default 100). |
| `DomainName`, `HostedZoneId` | Both given: the stack creates the four DNS records and the node issues a wildcard certificate by DNS-01 with the instance role. Only the domain: create the records listed in the `DnsRecordsNeeded` output yourself. Neither: sslip.io on the Elastic IP. |
| `AccessCidr`, `SshCidr` | Who may reach the four ports (default everyone; port 80 must stay open for HTTP-01 without a hosted zone) and, optionally, SSH. No SSH rule by default: the role includes Session Manager. |
| `SbctlVersion` | `latest` or a release tag. |
| `VpcId`, `SubnetId` | An existing VPC and public subnet, both or neither. |

The instance role may write only this bucket, put a value into only the claim-token secret, change records in only the given hosted zone (`ChangeResourceRecordSets`, `ListResourceRecordSets`, and `GetChange` on change IDs), and use Session Manager. User data prepares the volume, runs `install.sh` from the release with the stack's parameters, stores the claim token in the secret, and signals the stack with `cfn-signal` (with a `curl` fallback when the helper package cannot be installed). The stack waits 30 minutes for that signal and fails with the installer's last error when it comes. The installer log is `/var/log/sbctl-bootstrap.log` on the instance.

When the stack is ready, read the outputs: `DashboardUrl`, `ClaimUrl` and `ClaimTokenSecretArn`. Fetch the token with

```bash
aws secretsmanager get-secret-value --secret-id <ClaimTokenSecretArn> --query SecretString --output text
```

and use it on `ClaimUrl`. Creating the stack needs the "I acknowledge that CloudFormation might create IAM resources" box.

Checked: `cfn-lint` passes, and the user-data script passes `bash -n` with the template's substitutions applied. **Not deployed:** nothing here has run in an AWS account, so the first launch is the first test of the data-volume discovery, the `awscli` package on Ubuntu 24.04, the Secrets Manager write and the signal.

## Release signing

A release is signed by the maintainers' ed25519 key. Set it up once:

```bash
openssl genpkey -algorithm ed25519 -out sbctl-signing.pem
openssl pkey -in sbctl-signing.pem -pubout -out internal/selfupdate/release_key.pem   # commit this public file
gh secret set SBCTL_SIGNING_KEY < sbctl-signing.pem                                   # then delete sbctl-signing.pem
```

`.github/workflows/release.yml` runs on a pushed tag `vMAJOR.MINOR.PATCH[-suffix]`. It stops early if the committed key is still the placeholder, if the secret is missing or if the secret is not the private half of the committed public key. Then it runs `go vet` and `go test`, builds `sbctl-linux-amd64` and `sbctl-linux-arm64` (`CGO_ENABLED=0`, `-trimpath`, `-X main.version=<tag>`), builds Studio for both architectures with `studio/build.sh`, and `deploy/release-assets.sh` writes `SHA256SUMS`, signs it (`openssl pkeyutl -sign -rawin`, a raw 64-byte signature), stamps the public key into `install.sh` and copies `sbctl.yaml`. The job creates the release (a tag with a suffix becomes a pre-release) and attaches `sbctl-linux-*`, the Studio archives, `SHA256SUMS`, `SHA256SUMS.sig`, `install.sh` and `sbctl.yaml`.

To rotate the key: generate a new pair, commit the new public file, replace the secret, tag a release. Binaries from before the rotation verify only against the old key, so they cannot self-update to a release signed with the new one; a rotation needs a manual reinstall with the new `install.sh`. Nothing has been tagged or released from this repository by the tooling.

## Tests

`tests/linux/install-e2e.sh` (the `install-e2e` job of `.github/workflows/linux.yml`, Ubuntu 24.04 on amd64 and arm64) runs the whole path on a fresh VM:

- release signing with a throwaway key; `install.sh --verify-only` accepts it and refuses a tampered binary, a changed checksum list, a bad signature, a release signed by another key and the keyless repository copy, each without installing anything;
- `install.sh --binary` with `--tls off` and `--public-ip 127.0.0.1` (every name is `*.127.0.0.1.sslip.io`, reached with Host headers): units active and enabled, file modes, loopback-only ports, the dashboard host through the proxy (with the slim Studio artifact of the pinned upstream version as a stand-in for our platform build), sign-up refused;
- the claim: wrong token refused, claim works once and fails the second time, sign-in, an invite redeemed and removed;
- a personal access token, a project created through `POST /v1/projects`, its keys, a table created through `database/query`, a REST call through the proxy with the publishable key, Storage through the proxy, pooler logins on 5432 and 6543;
- a re-run changes nothing and restarts nothing, and a re-run with one flag changes that setting only;
- a re-run of `install.sh` with a v0.0.2 binary moves the daemon onto it (the daemon's `/proc/<pid>/exe` reports v0.0.2) without restarting shared services or projects;
- `sbctl self-update` against a local release server: refuses a tampered binary, a wrong key and an older signed binary under a newer tag, installs v0.0.3, restarts the daemon, leaves the project's Postgres running; then a release whose daemon exits on `serve` is rolled back to v0.0.3, whose daemon answers again;
- the claim token stays out of the installer's output when `--claim-token-file` is used.

Go unit tests: `internal/selfupdate` (signature, checksum, atomic replace, refusals, an OpenSSL-made signature fixture), `internal/api/claim_test.go` (the endpoint, single use, expiry, rate limit, concurrent redemption, invites, user removal; the Postgres store runs when `SBCTL_TEST_DATABASE_URL` is set), `cmd/sbctl/cmd_install_test.go` (flag to config mapping, minimal config rendering, `--set`, OS and glibc checks, EC2 metadata).

## Not done

- Uninstall: there is no `sbctl uninstall`. Stop and disable `sbctl.service` and the `sb-*` units, then remove `/var/lib/sbctl`, `/etc/sbctl` and the `sbctl` user by hand.
- `install.sh` resolves `latest` through a redirect of github.com and trusts TLS for that step only: the tag it gets is then used for signed files, so a wrong tag can only pick an older signed release.
- The claim page is at `api.<domain>/claim`, not `studio.<domain>/claim`: the Studio host belongs to Studio.
- No email: invites and the claim token are handed over out of band.
- The AWS stack is unverified in AWS (see above), and the Quick-create link needs a published template. `release.yml` has an optional `publish-template` job that uploads it when the repository variables `SBCTL_TEMPLATE_BUCKET` and `SBCTL_TEMPLATE_ROLE_ARN` are set; it is untested, and nothing sets those variables.
- The instance role of the AWS stack is reachable from every process on the instance through IMDS. The tenant-facing units deny `169.254.169.254` (`deploy/systemd/README.md`, cloud metadata); Postgres (WAL archiving) and Storage with the S3 backend cannot, and workstream J must keep the rule on the edge runtime.
