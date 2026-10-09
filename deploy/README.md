# deploy

How a Supavise node gets installed, claimed, updated and released. The [guide](../docs/guide.md) covers everyday tasks; this page has the operator detail.

| Path | What |
|---|---|
| `install.sh` | Bootstrap for any Ubuntu 24.04+ or Debian 12+ server: host checks, release download with signature and checksum verification, then `supavise install`. |
| `release-assets.sh`, `release-gate.sh` | Write the release manifest, sign a release (`SHA256SUMS` covers the manifest; `SHA256SUMS.sig`) and stamp the public key into `install.sh`; the gate refuses a tag whose commit has not passed ci, linux (the upgrade test), conformance and replication (the two-server test). |
| `releasetool/`, `MIN_UPGRADE_FROM`, `MIN_PEER_FROM` | Manifest, release notes and the pin edits of the nightly bump proposals (`go run ./deploy/releasetool`); the oldest version that upgrades straight to the release being built, and the oldest release a joined server may run beside it. |
| `cloudformation/supavise.yaml`, `aws/deploy.sh` | One-instance AWS stack (one required field, nothing retained silently) and its one-command deploy, attached to releases as `supavise-aws-deploy.sh`. |
| `systemd/` | The unit templates the binary embeds (`systemd/README.md`). |

The work happens in `supavise install` (`cmd/supavise/cmd_install.go`), not in shell, so it has unit tests and one flag set. `install.sh` exists because the binary has to be fetched and verified before it can run.

## Install on a server

Create the DNS records first ([DNS and TLS](#dns-and-tls)), then, as root:

```bash
curl -fsSL https://github.com/supavise/supavise/releases/latest/download/install.sh | sudo bash -s -- \
  --domain example.com --dns cloudflare --dns-credentials-file /root/cloudflare.env --email you@example.com
```

For a trial without a domain, leave `--domain` and `--dns` out: the node uses `<public ip>.sslip.io` and requests per-host certificates over HTTP-01.

`install.sh` checks the host (Ubuntu 24.04+ or Debian 12+, amd64 or arm64, glibc 2.35+, systemd), verifies the signature of the release's `SHA256SUMS` against the key stamped into the script, and checks `supavise-linux-<arch>` against the signed list. It refuses a binary whose `--version` does not name the release tag: the signature covers the checksums, not the tag, so an older signed binary attached to a newer tag would be a downgrade. A mismatch stops the install before anything changes. The repository copy of `install.sh` has no key; the one attached to a release does. `--binary PATH` installs a file you built instead (nothing verifies it). `--verify-only` downloads and verifies a release and installs nothing. Ubuntu 22.04 is not supported: its polkit 0.105 ignores the JavaScript rule that lets the `supavise` user manage its units.

`supavise install` creates the `supavise` user and directories; writes `/etc/supavise/config.toml` (0600, owned by `supavise`) with the settings that differ from the defaults; brings the host to what the release expects (`supavise system converge`: the systemd units and polkit rule, the `cluster` and `config.d` directories, mount protection for the data and config directories, the mesh port in ufw; `install-units` is its older name and does the same); opens TCP 80, 443, 5432 and 6543 in ufw when it is active; creates the system project (`supavise system init`: the Postgres and auth artifacts, the registry cluster, the dashboard GoTrue with sign-up disabled); starts the shared services (`supavise fleet start`: postgres-meta, Supavisor, Realtime, Storage, and Studio when the release carries it); enables `supavise.service`, which starts the project units and shared services at every boot; and prints the dashboard URL and, while nobody has claimed, the claim token.

Run it again at any time. A flag you leave out keeps its value in `config.toml`; the master key, the registry and the projects are never touched; a re-run that changes nothing restarts nothing, and one with a new binary restarts `supavise.service` onto it (compared by content) while shared services and projects keep running. `supavise install --print-config <flags>` shows the file a run would write.

### Flags

`supavise install --help` lists every flag. The ones most installs need:

| Flag | Meaning |
|---|---|
| `--domain example.com` | Base domain: `studio.<domain>` (dashboard), `api.<domain>`, `pooler.<domain>`, `<ref>.api.<domain>` (a project). |
| `--dns route53\|cloudflare\|hetzner\|digitalocean` | DNS-01 provider for a wildcard certificate. Credentials: `--dns-credentials-file` (`KEY=VALUE` lines, `api_token=...`) or `--dns-credential KEY=VALUE` (visible in the process list). On AWS the instance role is enough; `--dns-credential hosted_zone_id=Z...` skips the zone lookup. |
| `--email you@example.com` | ACME account contact. |
| `--s3-bucket B --s3-region R` | Keep WAL archives and base backups in S3 (AWS credential chain without static keys). `--s3-endpoint`, `--s3-path-style`, `--s3-credentials-file` for S3-compatible stores. |
| `--key-passphrase-file F` | Keep a copy of the master key and `config.toml` in the backup backend, encrypted with the passphrase in `F` (mode 0600, 12+ characters; nothing on the server stores it). Without it the summary reminds you to run `supavise system export-key` and keep the output offline. |
| `--auto-upgrade`, `--maintenance-window "Sun 03:00-05:00"` | Install new releases by itself inside the weekly window ([Update](#update)). Default window `Sun 04:00-06:00`. `--auto-upgrade=false` (or `supavise update config --mode notify`) switches back. |
| `--no-os-updates`, `--os-reboot never` | Skip unattended OS security updates (a new install sets them up); never reboot for them. |
| `--no-functions` | Run without Edge Functions. A new install turns them on; a re-run keeps the value in `config.toml`. |
| `--public-ip IP` | This server's public IP. Detected from the EC2 metadata service, then `checkip.amazonaws.com`, when omitted. |
| `--tls off` | Plain HTTP on 80 and 443, for tests or behind a TLS terminator. |
| `--set path=value` | Any `config.toml` setting, for example `--set ports.project_base=38000`. |
| `--aws-first-boot`, `--data-device /dev/…` | The first boot of an EC2 instance that a stack made with a data volume: find the volume (the one EBS disk besides the root disk; `--data-device` names it when there are several, and is refused when it is the root disk or a partition of it), make an XFS file system on it only when it is blank, mount it with project quotas, bind `/etc/supavise` to it and write the mount drop-ins, all before anything is read or written under `/etc/supavise`. A volume with data of another kind is never formatted. |
| `--join-token-file PATH` | Join the cluster whose leader printed the token in `PATH` (mode 0600) instead of creating a system project; `supavise node join` runs as the `supavise` user and returns when the server's copy of the registry streams and the leader has confirmed it, so the installer's exit means an active node. No project directory exists before the join. Run again on a server that follows a leader, the installer keeps it (no system project is created) and continues a join that stopped after its certificate. |
| `--claim-token-file PATH` | Write the token to a file (0600) instead of printing it, for unattended installs whose output is logged (the CloudFormation user data uses it). |

### DNS and TLS

With a domain, create these records, all pointing at the server (the installer prints them with your IP):

```
api.<domain>        studio.<domain>        pooler.<domain>        *.api.<domain>
```

The wildcard is required: Realtime and Storage resolve the project from the host name, so there is no path-based mode. With `--dns` and credentials, one wildcard certificate covers `*.api.<domain>`, `api.<domain>` and `studio.<domain>` (DNS-01). Without `--dns`, each host gets its own certificate on first request (HTTP-01), which needs every name to resolve to the server first. `internal/proxy/README.md` has the details. Running with TLS enabled accepts the certificate authority's subscriber agreement.

### Firewall and ports

Only 80, 443, 5432 and 6543 are meant to be reachable by clients. A server that belongs to a cluster also needs TCP 7443 open to the other servers (the mesh, mutual TLS): `supavise system converge` opens it when ufw is active, and the CloudFormation template opens it for the ranges in `PeerCidr1` to `PeerCidr3`. The shared services listen on more interfaces (`internal/fleet/README.md`, "Supavisor's API and shard listeners"), so a host firewall or security group that admits only those four ports is part of the install. With ufw active the installer opens them. `--firewall ufw` installs, enables and configures ufw (the SSH ports come from what sshd or `ssh.socket` listens on, then `sshd -T` and `sshd_config.d`, then 22); `--firewall none` leaves the host alone. With ufw installed but inactive (the default of several cloud images) and no `--firewall` flag, the installer stops and asks for one of the two; with ufw not installed it prints a warning. The CloudFormation template uses a security group.

### Custom domains and vanity subdomains

A project can answer on its own hostname (`api.acme.com`) and on a short name under your domain (`acme.api.<domain>`), as on hosted. Use **Project Settings**, **Custom Domains**, the CLI (`supabase domains`, `supabase vanity-subdomains`) or the Management API (`/v1/projects/{ref}/custom-hostname`, `/vanity-subdomain`). Owners and Administrators change them; other roles read them. The project's own `<ref>.api.<domain>` keeps working.

1. **CNAME.** Point the hostname at `<ref>.api.<domain>` (an A record to the server also works). The server checks that the name resolves from the server, not from the browser.
2. **Add** (`supabase domains create --custom-hostname api.acme.com`). The answer holds the TXT record to create: `_supavise-challenge.<hostname>` with a value starting `supavise-verify=`.
3. **Verify** (`supabase domains reverify`). The status moves from `2_initiated` to `3_challenge_verified` (TXT found, hostname not pointing at the server) and to `4_origin_setup_completed` (both found). Five attempts per project in a burst, then one every 15 seconds; past that, 429 with `Retry-After`.
4. **Activate** (`supabase domains activate`). The server checks DNS again, routes the hostname, requests its certificate at once (HTTP-01 or TLS-ALPN-01 on ports 80 and 443), restarts the project's Auth and reports `5_services_reconfigured`. Activation spends a verification attempt, and a record that has gone or moved sends the claim back to the status it now earns. `supabase domains delete` removes the route and certificate.

Only active hostnames get a certificate. A hostname belongs to one project while it is verified or active; a verified claim neither activated nor verified again within 24 hours stops holding the name. A project has one custom hostname, which cannot be `api.<domain>`, `studio.<domain>`, `pooler.<domain>` or anything under `api.<domain>`. Custom hostnames need per-host certificates, so they fail with `--tls dns01` (the API answers 400); with `--dns` and the default `auto`, each gets its own certificate beside the wildcard. Every custom hostname must resolve to the server before activation, since the certificate authority connects to it. Activation makes Auth build its links, OAuth redirect URIs and SAML endpoints from the hostname, as on hosted: add the new callback URL to each OAuth provider and update SAML providers that use the project's metadata. The token issuer (`iss`) stays `https://<ref>.api.<domain>/auth/v1`, so sessions and verifiers that pin it keep working. Storage's S3 endpoint signs the hostname the client used, so S3 clients work on it too.

A **vanity subdomain** (`supabase vanity-subdomains activate`) is one DNS label of up to 63 letters, digits and hyphens under the wildcard record, unique on the node. With `--dns` the wildcard certificate covers it; without a DNS provider (every `sslip.io` node), each name gets its own HTTP-01 certificate on activation, so the first request may wait for the certificate authority and every new name counts against its limits. Reserved names (`api`, `studio`, `pooler`, `www`, `admin`, `system`, and similar), names that look like a project ref and names another project holds are refused. A project uses a custom hostname or a vanity subdomain, not both: the second activation is refused with 409, and the vanity subdomain status then reads `custom-domain-used`. A vanity subdomain changes Auth the same way a custom hostname does.

`internal/domains/README.md` has the rules and `internal/proxy/README.md` the routing and certificate gate.

## The first administrator and more users

`supavise-gotrue@system` has public sign-up disabled, so nobody can create a dashboard account through Studio. The first administrator is created with the claim token the installer printed ([guide](../docs/guide.md#claim-the-node-and-sign-in)); the endpoint is `GET` and `POST /claim` on `api.<domain>` and on the loopback admin listener (`127.0.0.1:7000`).

- The token is single use and expires after 72 hours (`--claim-ttl`); the database keeps only its SHA-256. A failed request (the address exists, the password is refused) gives the token back. Passwords are 12 to 72 characters. Failed attempts are limited node-wide.
- While nobody has claimed, `sudo -u supavise supavise claim token` issues a new token and revokes the old one; `--if-none` issues nothing while an unused, unexpired token exists. After the claim it refuses unless you pass `--force` (an administrator locked out).

Later users come by invitation, with a role as on hosted (owner, administrator, developer, read-only; `internal/api/README.md` lists what each may do). Supavise sends no email unless `[mail]` is configured in `config.toml` (an SMTP relay for `supavise-gotrue@system`), in which case it also sends the invitation.

```bash
sudo -u supavise supavise users invite dev@example.com --role developer   # prints the sbi_... link (valid 7 days, works once)
sudo -u supavise supavise users invite ops@example.com --role administrator --org acme
sudo -u supavise supavise users invite qa@example.com --role read-only --project abcdefghijklmnopqrst
sudo -u supavise supavise users list                                       # who has which role
sudo -u supavise supavise users role dev@example.com administrator         # change a role (or restore an owner)
sudo -u supavise supavise users remove dev@example.com                     # ends their access at once, removes memberships and the access tokens they made
sudo -u supavise supavise orgs list
sudo -u supavise supavise orgs delete acme --yes    # deletes its projects (read replicas first, then a final backup each), members, invitations and SSO setup; without --yes it only lists them
```

An address with no account gets the claim page with the token filled in (the invitee picks a password and joins with the invited role); one with an account gets the dashboard's invitation page. An organization always keeps one owner (`users remove` refuses to delete the only owner without `--force`), and the node's last organization is never deleted.

### Single sign-on

People can sign in to the dashboard with the company's SAML 2.0 identity provider:

```bash
sudo -u supavise supavise sso add --metadata-url https://idp.example.com/saml/metadata --domain example.com --default-role developer
sudo -u supavise supavise sso info          # the ACS URL (https://api.<domain>/auth/v1/sso/saml/acs) and entity id for the identity provider
```

A person with an address of one of the domains joins the organization with the default role on first sign-in; anyone else waits for an administrator (`supavise sso pending`, `sso approve <email> --role developer`, `sso deny`, `sso allow <email>`). `sso list` and `sso remove <id|domain>` manage providers; removing one ends its users' sessions. Projects have their own identity providers for their end users: enable SAML in the project's Auth settings, then `supabase sso add --project-ref <ref>`. `internal/api/README.md` ("Single sign-on") has the rules.

## Update

You own the node, so Supavise tells you that a release exists and changes nothing until you ask, or until you opt in to automatic upgrades inside a maintenance window. A release is a tested bundle: the binary and the Supabase service versions it installs (`internal/versions/versions.yaml`). The [guide](../docs/guide.md#updates-and-maintenance) has the routine.

```bash
supavise update status               # settings, next window, latest release seen, last unattended upgrade
sudo supavise update config --mode auto --window "Sun 03:00-05:00"   # opt in (--mode notify opts out)
supavise upgrade --plan              # what a newer release would change (read-only, no root)
sudo supavise upgrade                # move the whole node onto it (below)
sudo supavise rollback               # and back to the previous release
sudo supavise self-update            # replace the binary only (below)
```

### The routine

One loop, whether you run it or the maintenance window does. The sections below hold the detail of each step.

1. **Check.** The daemon asks GitHub for the newest release once a day and raises `update_available` once per version ([below](#health-alerts-and-maintenance-notices)); `supavise update status` shows what it found and `supavise upgrade --check` looks again. Nothing is installed.
2. **Upgrade.** `supavise upgrade --plan` shows what the release changes and restarts. `sudo supavise upgrade` moves the whole node: the binary, the shared services, then the projects' GoTrue and PostgREST in a canary-first rollout ([below](#upgrading-the-node-supavise-upgrade)). With `mode = "auto"` the maintenance window runs the same command as `supavise upgrade --unattended`, which refuses unless the node is healthy, backed up and its master key has a copy in the backup backend.
3. **Verify.** The upgrade checks `supavise status` itself and rolls back if the node is worse than before. You hear about it: `upgrade_started` when the node is about to change, then `upgrade_succeeded` or `upgrade_failed` (a rolled-back upgrade is a warning, one that needs you is critical), to the same webhooks and email as the other alerts, also when the maintenance window started it. `supavise status` shows an upgrade that is running and any project whose restart an earlier upgrade held back, as notes (they do not make the node degraded); `sudo supavise upgrade` finishes the held-back restarts.
4. **Roll back.** A failed upgrade goes back by itself (exit status `3`). To go back after a good one, `sudo supavise rollback` puts the previous kept release on the node, with the projects the upgrade moved. Registry migrations only go forward, so a release that added some needs the system cluster restored first (the command says so).
5. **Auto mode.** `sudo supavise update config --mode auto --window "Sun 03:00-05:00"` turns step 2 into the window's job, once per window; a failure pauses it until `sudo supavise update resume` ([How it runs](#how-it-runs)). `--mode notify` turns it off again.
6. **OS patches.** Security updates install by themselves (`unattended-upgrades`); the reboot some of them need happens only inside the window, only on a healthy and quiet node, never while an upgrade or self-update holds the host lock, and never with `os_reboot = "never"` ([below](#operating-system-updates)).

### Settings

The `[update]` section of `config.toml`. `supavise update config` changes it (it writes `config.toml`, rewrites the timer and applies the change without restarting the daemon; a new `check_interval` takes effect at the next `supavise.service` restart). The installer's flags set the same keys.

| Key | Values | Meaning |
|---|---|---|
| `mode` | `notify` (default), `auto` | `notify` only tells you (the daemon raises `update_available` once per release). `auto` runs `supavise upgrade --unattended` inside the window, and only there. |
| `window` | `"Sun 04:00-06:00"` (default) | Weekly maintenance window in the node's time zone ([format](#the-window-format)). |
| `channel` | `stable` | The only channel: GitHub's latest release that is not a pre-release. |
| `check_interval` | `24h` (default); a duration (`12h`), whole days (`2d`) or seconds (`7200`), one hour at the least; `off`, `never` or `0` | How often the daemon asks GitHub for the latest release. `off` stops the check; `auto` mode still upgrades in the window. `SUPAVISE_UPDATE_CHECK_INTERVAL` overrides it. |
| `os_security_updates` | `true` on a new install | Unattended OS security updates ([below](#operating-system-updates)). An existing node keeps `false` until you turn it on. |
| `os_reboot` | `window` (default), `never` | Reboot the node inside the window when an OS patch needs it. Only applies when `os_security_updates` is on. |

### How it runs

`supavise-upgrade.timer` starts `supavise-upgrade.service` (`supavise update run`, as root). `supavise system converge` enables the timer when the node has something for it to do: auto mode, or the reboot in the window. It wakes the service when the window opens and, in auto mode and for the OS reboot, every 15 minutes until it closes (at most 16 times, so a long window gets a wider step). The service does nothing outside the window and does not check for releases (the daemon does, see [Health, alerts and maintenance notices](#health-alerts-and-maintenance-notices)). In auto mode it runs `supavise upgrade --unattended` once per window, and the exit status decides what comes next:

| Exit | Meaning | Then |
|---|---|---|
| `0` | upgraded, or nothing to do | the window's attempt ends |
| `2` | a pre-check refused, nothing changed | tried again at the next wake-up in the window |
| `3` | failed and rolled back | the unit fails (`systemctl --failed` shows it) and that release is **skipped** until a newer one exists (`sudo supavise update resume` lifts the skip) |
| `4`, or any other result | failed, needs the operator | **automatic upgrades pause** until you have looked and run `sudo supavise update resume` |

Attempts log `unattended_upgrade_started` and `unattended_upgrade_result` to the journal (`journalctl -u supavise-upgrade`). `supavise upgrade` raises its own `upgrade_*` alerts (exit `3` and `4` included); `update run` adds one critical `upgrade_failed` for the two outcomes that command cannot report, an upgrade that could not run (or ended in a status the table does not name) and one that was cut short, each saying automatic upgrades are paused until `sudo supavise update resume`. An attempt the service skips logs `unattended_upgrade_skipped` with a `reason`: `blocked` (automatic upgrades are paused), `rolled_back_release` (the skip above) or `window_closing` (the cutoff below). A window the node slept through is skipped, not made up. The last hour of the window starts nothing, because an upgrade runs to its end and for 50 projects that can outlast the window (less than half of a window shorter than two hours); with the default window the last start is 05:00, and an upgrade started then may run past the window close. Choose a window long enough for the upgrade plus the hour. An upgrade that never reported (the node crashed, something killed the service) is not run again: the next wake-up logs `unattended_upgrade_interrupted`, sends a critical `upgrade_failed` (the operator heard `upgrade_started` and would otherwise never hear the end), fails the unit and pauses automatic upgrades until `resume`. Stopping the unit (`systemctl stop`, a reboot) lets the upgrade end, for up to 50 minutes (`TimeoutStopSec=1h`, `KillMode=mixed`, no start timeout). The service remembers its state (the window that had its attempt, the pause, the rolled-back release, the last result, the last reboot) in `state.json` in `/var/lib/supavise-upgrade`, the unit's `StateDirectory=`, owned by root. The command takes the directory from `$STATE_DIRECTORY` (a person running it by hand gets the same path), never from `config.toml`, so a custom `state_dir` does not move it. It creates no directory except `/var/lib/supavise-upgrade` itself when run by hand before the timer ever ran, and stops if the directory is missing, a symlink, owned by another user or writable by others. `internal/update/README.md` has the rules.

**What the root service still trusts.** The service runs as root and `config.toml` belongs to the `supavise` account, so the account can change what the service reads from it: `[update]` (turn automatic upgrades on, move the window, set `os_reboot`), `[alerts]` and `[mail]` (where the upgrade's alerts go; root sends them), and `state_dir` (where `supavise upgrade` reads the registry's socket and writes the upgrade marker and the alert state; only the update record above is pinned). When it runs as root the alert store refuses to follow a link there: `<state_dir>/system` must be a real directory owned by root or by the owner of `state_dir`, and the lock and state files are opened without following links. The account cannot change the binary that runs (`bin_path` must be `/usr/local/bin/supavise`, else `supavise upgrade` refuses), which releases install (signed with the keys compiled into the binary, along the manifest's `min_upgrade_from`), or whether the unattended gates apply. The reboot gate asks the registry through `supavise projects list --json` as the `supavise` user, so it protects the node from a reboot at a bad moment, not from that account.

With `os_security_updates` and `os_reboot = "window"` the service also reboots the node when an OS update needs it, once per window. It holds the reboot back (an `os_reboot_deferred` event names the reason) when the upgrade in the same run failed or was refused; when less than 15 minutes of the window remain; or when the node is not healthy or quiet: `supavise.service` is not running, a `supavise-*` unit has failed, a base backup is running, a project is in any status but `ACTIVE_HEALTHY`, `INACTIVE` or `REMOVED`, or a `supavise upgrade`, `rollback` or `self-update` holds the host lock (`/run/supavise-maintenance.lock`). The service takes that lock itself before the checks (after its own upgrade has returned, since the upgrade takes it in its own process) and keeps it until the machine is going down, so none of those can start between the check and the reboot; it does not wait for the lock and tries again at the next wake-up. The check reads the clock again just before the reboot, because an upgrade can outlast the window. Only a quiet node is rebooted because a project's units are not ordered after `supavise.service`, so systemd stops them at the same time as its 10-minute drain; the reboot does not count on the drain. `sudo supavise update run` by hand acts for real; `--dry-run` changes nothing.

### The window format

`[DAYS ]HH:MM-HH:MM`, 24-hour, in the node's time zone (the zone of `/etc/localtime`, UTC on most cloud images; `timedatectl` shows and sets it). Days are `Mon Tue Wed Thu Fri Sat Sun` in any case (full names work): `Sun 03:00-05:00`, a list (`Sat,Sun 02:00-04:00`), a range (`Mon-Fri 01:30-03:30`; `Fri-Mon` wraps around the weekend), or `daily 03:00-04:00` and `03:00-04:00` for every day. A window that ends after midnight belongs to the day it opens on (`Sun 23:00-01:00` is Sunday 23:00 to Monday 01:00). The default sits after the nightly 03:00 base backups. A window must close after it opens.

### Operating system updates

A new install turns on unattended-upgrades for **security updates only**, on Ubuntu 24.04+ and Debian 12+ (an existing node is not changed). `supavise install` (and `supavise system os-updates`) installs `unattended-upgrades` and `needrestart` when missing and writes `/etc/apt/apt.conf.d/52supavise-unattended-upgrades`: daily updates from the security origins only (Ubuntu's `-security` and ESM pockets; Debian's `-security` archive, not its point-release updates). `apt-config dump` and `unattended-upgrade --dry-run --debug` show what it reads. `--no-os-updates` on a re-run, or `supavise update config --os-security-updates=false`, removes that file and leaves the packages and the needrestart setting.

unattended-upgrades does not reboot (its `Automatic-Reboot` picks a time of day, so the file sets it to `false`); the service above reboots inside the window, learning that a reboot is due from `/run/reboot-required` (Ubuntu) and `needrestart -b -k` (kernel status 2 or 3; Debian writes no marker). `/etc/needrestart/conf.d/50-supavise.conf` tells needrestart to leave every `supavise-*` unit alone, because it would otherwise restart a project's Postgres in the middle of the day; the unit picks up the new library at the next reboot or Supavise upgrade. The file stays whenever needrestart is installed, also after an opt-out. It uses the documented `override_rc` form, but no test restarts a service through needrestart to confirm it is honored.

**Expected reboot impact.** A reboot stops every project, then `supavise.service` starts them one at a time. [Footprint](../docs/reference/footprint.md) measured `supavise system start` on idle projects (GitHub `ubuntu-24.04`, 4 vCPU, 16 GB): 5 s at 10 projects, 12 s at 25, 24 s at 50, the same on arm64. The machine's own shutdown and boot come on top (not measured), and large databases or WAL to replay take longer. `supavise.service` waits up to 10 minutes for running lifecycle operations before it stops. Set `os_reboot = "never"` if a person must choose the moment (`supavise update status` then says "a reboot is waiting").

### Upgrading the node: `supavise upgrade`

```bash
supavise upgrade --check                     # is there a newer release? (no root, changes nothing)
supavise upgrade --plan                      # what would change and restart (no root, changes nothing)
sudo supavise upgrade                        # shows the plan, asks, upgrades
sudo supavise upgrade --yes --version v1.4.0
sudo supavise upgrade --yes --include-postgres   # also move the projects' PostgreSQL
sudo supavise upgrade --unattended           # what the maintenance window runs; never asks
sudo supavise rollback                       # back to the previous kept release
```

`supavise upgrade` moves the node onto a release the way hosted Supabase moves its platform: the node's own services first, then each project's GoTrue and PostgREST in a rollout. A project's PostgreSQL release moves only with `--include-postgres` (or later, per project, from Studio or `supavise projects upgrade`), and each move restarts that PostgreSQL. It verifies the signature, the signed manifest and the new binary, and refuses a jump the manifest does not allow (`min_upgrade_from`). The plan lists what changes, what restarts (the daemon: HTTPS fails for a few seconds; Realtime: websockets drop; Storage: uploads in flight fail; each project's GoTrue and PostgREST: a minute or two offline; Supavisor: pooled connections drop) and the projects it moves or skips. Before anything stops it fetches the artifacts, checks the disk and takes a base backup of the system project (the registry) and of every running project (not a paused one); a failure there exits 2 with the node untouched. Then it swaps the binary in (keeping the old one), restarts the daemon, which moves the shared services one at a time, and rolls the projects: `canary_projects` first, then `batch_size` at a time, stopping at the first failure. A project that cannot be upgraded (paused, unhealthy, an extension the new PostgreSQL cannot serve, already newer than the release) is skipped and listed. A release can also render a project's PostgreSQL, GoTrue or PostgREST files differently without moving a release; the same rollout restarts those units, so the plan cannot list them. A PostgreSQL setting saved without a restart is not applied by the rollout unless the project restarts for the release anyway. A rollout cut short is finished by the next `supavise upgrade`.

If the new daemon does not answer, a shared service does not come up, a project fails or the node is worse afterwards, the upgrade rolls back by itself: the projects it moved go back to the releases they ran, and the previous binary and units come back. Exit status: `0` upgraded or nothing to do; `2` refused by a check, nothing changed; `3` failed and rolled back; `4` failed and the node needs you (the message says what state it is in). `--unattended` implies `--yes` and refuses (status 2) unless `supavise status` says healthy, every running project has a backup newer than 24 hours and the master key has an encrypted copy in the backup backend (`supavise system escrow-key`); by hand, a missing key copy is a warning. While it runs, `<state_dir>/system/upgrade.json` holds `phase`, `from`, `to`, `started_at`, `pid` and `detail`, and `supavise status` shows it; it stays with a final phase (`done`, `rolled_back`, `failed`, `refused`). A second upgrade refuses while the first lives, and the command (and `supavise rollback`) holds the host lock so the OS reboot never lands in the middle.

**Three layers.** An upgrade brings a node forward in three layers that need no particular order. The release is the binary, the shared services and the projects, as above. The host layer is `supavise system converge`, a list of idempotent steps (units and the polkit rule, directories, mount protection, the mesh port in ufw, the cluster settings from the leader on a server in a cluster, declared packages) that prints what it changes; `supavise system converge --check` (no root, `--json` for tools) lists what is pending (`supavise status` shows the same list as its Host block, with the steps it could not check, and does not change its verdict). The upgrade runs converge right after the binary is swapped, before the daemon restarts, and a failure of it fails the upgrade and rolls it back (an upgrade that swaps nothing converges after the backups and says to restart the daemon, which reads the host's state only at its start). Converge restarts no project and no service. It is also what an old v0.1.x driver runs on the new binary, as `install-units`, at the end of the first hop; there a failure is a warning, and the daemon raises `host_not_converged` while `/var/lib/supavise/converged` is behind the release's revision. The stack layer is the AWS stack, below.

On AWS the plan also shows how far the stack is behind the release (`Infrastructure  AWS stack "supavise" is at revision 1; this release needs 2 for: ...`). `sudo -E supavise upgrade --aws [--stack-name N] [--set Failover=on]` brings the stack forward first, before anything on the node changes: it fetches the release's signed script and template (checked against the signed list; the variables that replace what the script trusts, which are for tests, are kept out of the script's environment: the release key, the download address and the metadata address) and runs `supavise-aws-deploy.sh update` with your own credentials, which `sudo -E` keeps; the node's instance role never changes its stack. The script shows a change set, refuses a replacement or a removal of anything that matters, and asks you to type `apply`; its exit status 2 (refused) or 3 (failed) stops the upgrade with the node unchanged. Without credentials of yours in the environment the node side goes ahead and the command is printed, both for the node (`sudo -E supavise upgrade --aws ...`) and for a shell that holds the credentials but is not the node (`supavise-aws-deploy.sh update --stack N --region R`, the script that is attached to the release). A timer's run never changes the stack: it raises `infra_behind` and goes on. `deploy/aws/README.md` has the script.

**In a cluster.** An upgrade is of the node it runs on. It plans, backs up and rolls out only the projects homed there (the plan names the ones homed on other servers, which are upgraded where they run), and a follower leaves the system project's backup to the leader. A follower holds the registry read-only, and the commands that back up, upgrade or restart a project open it for writing, so none of them runs on a follower. A follower with no project running of its own, the usual one because it holds replicas of the leader's projects, upgrades its binary, its shared services and its host layer, takes no backup and runs no rollout. A follower with a running project of its own is refused (exit status 2, nothing changed), and the refusal names the projects: move each to the leader with `supavise projects failover <ref>`, run on the leader, or pause it, then run the upgrade again. Routing a follower's project through the leader is not done. On the leader, `supavise upgrade` and `supavise rollback` announce maintenance (`cluster.maintenance`) before they stop anything, renew it while they run and clear it when they end, so an automatic failover (`[failover] mode` set to `project` or `server`) does not fire while the leader's daemon or its system PostgreSQL restarts; an announcement that cannot be made (a planned failover holds it) stops the run before it changes anything, and one left by a run that was killed expires after 15 minutes. A release whose manifest says `wal_compat: false` (its PostgreSQL cannot read the WAL of the releases before it, or the other way) is refused on a server that holds the primary of a database while another server holds a standby of it on an older release: upgrade the servers that hold standbys first (`supavise upgrade` there), then this one. The refusal names them. `wal_compat` is absent, which reads as true, for a release that keeps the WAL format, and `deploy/release-assets.sh` writes `false` when `SUPAVISE_WAL_COMPAT=false` is set for the release. `supavise upgrade --drop-replicas` does not exist: a major version upgrade is refused whatever the replicas are (`upgrades across major versions are not supported yet`), so removing replicas would only destroy them. Remove them with `supavise replicas rm` when you want that.

The upgrade and the rollback send alerts from their own process (as root, outside the daemon, so they go out when the daemon is down): `upgrade_started` once the artifacts are fetched and the backups are taken and before the binary changes (a run refused earlier changes nothing and says nothing), `upgrade_succeeded`, and `upgrade_failed` for a rolled-back upgrade (warning, naming the project the rollout halted at) or one that needs you (critical). They carry the from and to versions and say when the maintenance window started the run. The daemon sends the same three kinds for one project's upgrade (Studio's "Upgrade project" and `POST /v1/projects/{ref}/upgrade`), with the project's ref. The first upgrade from a release that does not send them sends none, because the installed binary drives the upgrade.

**Rollback.** `supavise rollback` goes back to the previous kept release (binaries in `/usr/local/lib/supavise/releases/<version>/`, owned by root): its binary and units, its service versions, and the projects the last upgrade moved, not ones an Owner upgraded later; an upgrade that did not change the binary reverts only the projects it moved.

- Registry migrations only go forward, so the rollback refuses (status 2) when the registry holds a migration the old release does not know. Restore the system cluster from its pre-upgrade base backup by hand, with the control plane stopped (`internal/backup/README.md`, "Disaster recovery of the system cluster"; `supavise backups restore` refuses the system project), then run the rollback again. An upgrade that fails after its new daemon applied migrations stays on the new binary and exits 4, after it put the projects it moved back. The plan says so for a release that adds migrations.
- When the old daemon starts it restarts, one after another, each project whose PostgreSQL, GoTrue or PostgREST files the rollout had already restarted onto the new release's files, and each restart drops that project's connections (a previous release built before these marks restarts every project whose files differ).
- GoTrue's migrations stay applied; the older GoTrue runs on the newer schema, and the pre-upgrade backup is the way back for the data.

| Key in `[upgrade]` | Default | Meaning |
|---|---|---|
| `canary_projects` | 1 | projects upgraded one at a time first (smallest database first); a failure stops the rollout. `-1` for none |
| `batch_size` | 5 | projects upgraded at once after the canaries |
| `keep_releases` | 3 | releases whose binary and artifacts stay on disk (the current one counts), for `supavise rollback`; unused artifacts go too |

**Moving a v0.1.x node.** A release with cluster support renders an existing project's Postgres, Auth and REST units exactly as v0.1.1 did (a test pins it), so moving a v0.1.x node onto it restarts no project unless the release also moves a service version. The installed binary drives the first hop, so run `sudo supavise upgrade` once to land the new binary, then `sudo -E supavise upgrade --aws` for the stack.

**Two servers.** Upgrade each server with `sudo supavise upgrade`, the leader first when the release adds registry migrations: they only add, so a follower one release behind runs against the leader's newer registry, and a join accepts a release only within one minor of the leader's. `supavise upgrade` does not set the cluster's maintenance announcement. While the two servers run different releases, automatic failover waits, because the release-match gate is closed, and a failover or switchover by hand refuses nodes on different releases unless you pass `--force`. A follower cannot roll out a project homed on it, because that writes upgrade rows in the registry, which a follower holds read-only; the engine refuses it.

`internal/nodeupgrade/README.md` has the code's rules.

### Replacing the binary by hand: self-update

`sudo supavise self-update [--version v1.2.3]` (and `supavise self-update --check`) verifies the ed25519 signature of `SHA256SUMS` against the public keys compiled into the binary (`internal/selfupdate/release_key.pem`, and `release_key_next.pem` during a [key rotation](#rotating-the-release-signing-key)), checks the signed [release manifest](#the-release-manifest) and the binary against their checksums, and refuses a release that needs an older version than you run to go first (`min_upgrade_from`). It replaces `/usr/local/bin/supavise` with one rename (the previous binary stays as `supavise.prev`), refreshes the units and restarts `supavise.service`, then waits up to `--wait` (5 minutes) for the daemon to answer on its admin listener. If it does not, it puts the previous binary back and restarts the service. Project units keep running. Artifact versions move with a release, not through this command. A binary built without a committed release key refuses to self-update.

### Project versions

A release pins the Postgres, GoTrue and PostgREST versions that new projects get. Existing projects keep what they run until their Owner or Administrator upgrades them, as on hosted: in Studio under Settings > General, "Service versions", with `POST /v1/projects/{ref}/upgrade`, or on the node:

```bash
supavise projects versions [<ref>]         # what each project runs, whether an upgrade is available, the last upgrade's outcome
supavise projects upgrade <ref>            # shows the plan, asks, upgrades one project
supavise projects upgrade --all --yes      # every eligible project: canary first, then batches; stops at the first failure
supavise artifacts gc --dry-run            # which unused artifacts would go
```

Each upgrade takes a fresh base backup first (reason `pre-upgrade`) and changes nothing if that fails; GoTrue's migrations only go forward, so that backup, not the rollback, is the way back for the data. If a restarted service does not come up, the previous versions start again; the project is offline for about a minute, longer when PostgreSQL restarts and recovers. Upgrades across Postgres major versions are refused, and so is a move to an older release than the project runs (after a binary rollback, such a project stays on the newer release and `supavise projects versions` lists it as ahead of the node). When the Postgres release changes, each installed extension is checked against it first, and an upgrade the release cannot serve is refused with the extension named, before anything stops; the upgrade never runs `ALTER EXTENSION UPDATE`. `supavise projects upgrade` ignores a dropped SSH session, and a project left `UPGRADING` by a killed upgrade is started again on its previous versions by the daemon. Artifacts of the previous release stay for `keep_releases` releases and while any project runs them. `internal/lifecycle/README.md`, "Service versions and project upgrades", has the details.

## Health, alerts and maintenance notices

```bash
sudo -u supavise supavise status           # one-line verdict, a table of components, the projects that need attention
sudo -u supavise supavise status --json    # the whole report
curl https://api.<domain>/healthz          # {"status":"healthy"}, for an uptime monitor
```

`supavise status` checks the daemon, the edge, the system cluster, each shared service and every project, the age of each project's newest base backup, free space, the certificates, whether the backups hold an encrypted copy of the master key, and whether a newer release exists. The exit status is 0 for healthy, 1 for degraded (the node serves, something needs attention) and 2 for down (the daemon, the edge or the system cluster is not running). When `status` cannot check at all (an unreadable config, a user who may not read the node's files) it prints an error and exits 1, so a script that must tell "degraded" from "could not check" reads the `status` field of `--json`. A newer release, a master key without a backup copy, an upgrade that is running (its phase, the releases and how long it has run) and projects whose restart an upgrade held back (with the command that finishes them, `sudo supavise upgrade`) are notes and do not change the verdict.

`GET /healthz` on the API host needs no credentials and answers `{"status":"healthy"}`, `{"status":"degraded"}` or `{"status":"down"}` and nothing else. The daemon reuses a report for 20 seconds. It is a 200 unless the node is down, so a load balancer does not pull a node because one project's PostgREST stopped; a monitor that matches `"healthy"` sees degraded. `GET /healthz/detail` returns the whole report to an Owner or Administrator, limited to the projects of the organizations the caller owns or administers.

The daemon sends alerts to webhooks and email:

```toml
[alerts]
email_to = "ops@example.com"                 # through [mail]
[[alerts.webhooks]]
url = "https://hooks.example.com/supavise"
secret = "a long random string"              # optional: signs timestamp and body (X-Supavise-Signature, X-Supavise-Timestamp)
```

The alerts are `disk_low`, `backup_failed`, `project_unhealthy`, `certificate_expiring`, `node_unhealthy` and `update_available` (once a day the daemon asks GitHub for the newest release and records it in `<state>/system/update.json`; it installs nothing). A problem is sent once (after lasting three minutes), again after 12 hours, and once more when it clears; at most 20 notifications go out in an hour. The upgrade commands and the daemon's project upgrades also raise `upgrade_started`, `upgrade_succeeded` and `upgrade_failed` ([Upgrading the node](#upgrading-the-node-supavise-upgrade)); they are announcements, never quieted or de-duplicated, and a maintenance window or a running upgrade does not hold them back (it does quiet `project_unhealthy` and non-critical `node_unhealthy`). `host_not_converged` (the host layer is behind the binary: run `sudo supavise system converge`) and, when an unattended upgrade finds the AWS stack behind, `infra_behind` are conditions that any node can raise. With a second server, the daemon also raises these: `replica_unhealthy`, `replica_lag` and `replica_capacity` for a replica; `node_unreachable`, `node_version_skew` and `standby_behind` for a peer or the registry; `failover_started`, `failover_completed` and `failover_failed` for each move; `failover_auto_off` when an automatic mode is set and this node cannot fence; and the critical `fenced` when this node lost the leadership and starts no database. The three `failover_*` announcements are never held back by the hourly cap, as the `upgrade_*` ones are; `fenced` is critical, so the cap does not hold it back either. `sudo -u supavise supavise alerts test` sends a test alert to every destination. Thresholds are in `[health]`, the rest in `[alerts]`; see `internal/health/README.md` and `internal/alerts/README.md`.

`supavise maintenance announce --at "2026-10-12 22:00" --duration 2h --message "Database maintenance"` records a window that `supavise status` and `/healthz/detail` show while it is open; `supavise maintenance clear` removes it. A window longer than 24 hours needs `--allow-long`, and a window that was not extended stops quieting alerts 24 hours after it starts. An upgrade marker counts only while it is under two hours old. While a window is open or an upgrade runs, `project_unhealthy` and non-critical `node_unhealthy` are neither raised nor resolved; a critical `node_unhealthy` (the system cluster or the registry down), `disk_low`, `backup_failed` and `certificate_expiring` are still sent. The dashboard banner stays empty: Studio draws any incident as "We are investigating a technical issue" with a link to Supabase's status page and does not show the message, so tell dashboard users about planned downtime another way. `internal/notice/README.md` has the details.

## Storage on S3

Storage keeps its objects as files under `<state_dir>/system/storage` by default (`[fleet] storage_backend = "file"`). With `"s3"` it keeps them in a bucket that you own. A failover of the whole server needs the bucket, because files on one server's disk cannot follow the leader. A failover or switchover of one project does not.

`supavise storage migrate --to s3` moves the objects of every project while Storage keeps serving. Run it as the `supavise` user on the leader:

```bash
sudo -u supavise supavise storage migrate --to s3 --bucket my-objects --credentials-file /root/storage-s3.env   # a bucket with a static key
sudo -u supavise supavise storage migrate --to s3 --bucket <ObjectsBucket> --role-arn <StorageRoleArn>        # on AWS, with the stack's role
sudo -u supavise supavise storage migrate --to s3 --status
```

The bucket's endpoint, region and addressing come from `[fleet] storage_s3_endpoint`, `storage_s3_region` and `storage_s3_force_path_style`; the region defaults to `[backup]`'s. A secret is never an argument. The credentials file has `access_key_id=` and `secret_access_key=` lines and a mode that only its owner can read; the key lands in `/etc/supavise/config.d/30-storage-s3.toml` (0600), never in `config.toml`. With `--role-arn`, the config gets `[fleet] storage_s3_role_arn`, the daemon assumes the role with the instance's credentials and serves the short-lived result to `supavise-storage` on `127.0.0.1:[fleet] storage_credentials_port` (4010). No key is stored. The role and a static key exclude each other.

The daemon reads `storage_s3_role_arn` when it starts. On AWS the installer's first boot (`--aws-first-boot`) writes it to `config.toml` from the instance's `supavise:storage-role` tag, unless the file names a role or a static key already, so a server made by the stack serves Storage's credentials from its first start and the migration needs no credential flag. A server installed before that, or without the tag, has a daemon that started without the role and cannot serve Storage's credentials: Storage starts on the bucket and fails to sign its first requests, and the command says to restart `supavise.service` and run `--resume`. Avoid that by setting the role and the bucket first and restarting the daemon, then migrate with no credential flag:

```toml
[fleet]
storage_s3_role_arn = "<StorageRoleArn>"
storage_s3_bucket = "<ObjectsBucket>"
```

```bash
sudo systemctl restart supavise.service
sudo -u supavise supavise storage migrate --to s3
```

A node that joins later takes `[fleet] storage_*` from the leader (they are cluster-scoped), and its daemon assumes the same role with its own instance role.

What the command does:

1. It checks that Storage answers and that the credentials can put, get and delete an object in the bucket.
2. It copies every project's files to the bucket (`--rate-limit` MiB per second, 32 by default) and repeats catch-up passes until one takes under a minute. Storage's S3 key, `<ref>/<bucket>/<name>/<version>`, equals the file's path, so each file is one object with its content type and cache control. Names that cannot be an S3 key are not copied and are listed in `--status`.
3. It checks, for each project whose database runs, that every row of `storage.objects` has its object in the bucket with the recorded size. A mismatch stops the run before anything switches.
4. It switches. Writes to Storage answer `503` with `Retry-After: 5` while a last copy runs; reads continue. Then `supavise-storage` stops and reads fail until it starts again on the bucket. That takes more than a restart (5 to 15 seconds), because a last pass looks at every file once and reads every running project's rows, so it grows with the store. The command prints how long it lasted.
5. It keeps the files as `objects.migrated-<date>` and does not delete them. `--cleanup` does, once the migration is done.

A run that stopped for any reason continues with `--resume`. `--rollback` copies what the bucket gained back to the files and switches back. On AWS, versioning on the objects bucket (30 days of noncurrent versions) takes the place of the nightly copy of Storage's files, which the backup skips while Storage uses S3. On a node that uses a role, the first render after a reboot restarts Storage once (5 to 15 seconds), because the endpoint's token changes at boot.

## Add a replica server

A second server gives a project a read replica, a standby of the registry and a place to move projects to. [Read replicas and failover](../docs/reference/replicas.md) has the names, the failure matrix and the data a failure can cost. This section adds the server.

Before you start:

- The leader's `[backup]` must be an S3-compatible bucket. The new server reads its first copy of the registry and of each replica from it. `supavise node token` refuses with a `file://` backend.
- TCP port 7443 must be open between the servers, in both directions if you can: a session works from whichever side can connect. The mesh uses mutual TLS with a certificate authority derived from the master key. With ufw active, `supavise system converge` opens the port. A cloud firewall or security group is yours to open.
- The new server runs Ubuntu 24.04+ or Debian 12+ and has not been installed as a node of its own. Its release must be within one minor of the leader's, with the same major.
- The host layer of both servers is converged: `sudo supavise system converge --check` lists nothing pending. A server whose host layer lags its binary keeps the cluster features off, and `supavise node token` and `node join` refuse until `sudo supavise system converge` has run. A daemon that started before the converge keeps them off until it restarts.
- The master key is copied to the new server, so a break-in on either one exposes both. The cluster certificate authority derives from the same key and adds no new root.

**On any Linux server.**

1. On the leader, create a token. The token goes to standard output and the hints to standard error, so redirect it:

   ```bash
   (umask 077; sudo -u supavise supavise node token --region eu-west-1 --ttl 1h > join-token)
   ```

   The token holds the leader's address, the fingerprint of the cluster CA and a secret. The leader keeps only the secret's hash. The token works once and expires after `--ttl` (1 hour by default). The first token also gives the leader its cluster identity, and the daemon restarts once to listen on port 7443. `--name` limits the token to one node name, and `--region` is the region of the new server, one that Studio knows.
2. Copy the file to the new server (mode 0600, for example `/root/join-token`) and install with it:

   ```bash
   curl -fsSL https://github.com/supavise/supavise/releases/latest/download/install.sh | sudo bash -s -- --join-token-file /root/join-token --firewall ufw
   ```

   The installer prepares the host without creating a system project, then runs `supavise node join` as the `supavise` user and converges the host again, now that it has its cluster identity. Before the join spends the token it downloads the Postgres release (nothing else downloads artifacts for a server that creates no system project); after the exchange it downloads the other services' artifacts, which a follower runs or keeps ready for a promotion, and serves the WAL relay of the system cluster itself, because the first copy replays the archive before it streams and no daemon runs yet. The server receives its certificate, the master key, the cluster's settings and the first copy of the registry. The domain, the TLS settings and the backup settings come from the leader (`/etc/supavise/config.d/10-cluster.toml`; change them on the leader). The node is `joining` until its copy streams, then `active`.
3. On the leader, check that the node is `active`:

   ```bash
   sudo -u supavise supavise node ls
   sudo -u supavise supavise status
   ```

   `supavise node ls --dns` prints the DNS records the cluster still needs. Any server answers any project, replica or balancer host and forwards when it does not run the target, so no record is needed to start. Point `<identifier>.api.<domain>` at the replica's server later to keep reads local.
4. Add a replica in Studio (**Project Settings**, **Infrastructure**), with `supavise replicas add <ref> --region eu-west-1`, or for every project with `[replicas] default = "all"` in the leader's `config.toml`. `supavise replicas ls` shows the seven setup steps. Each replica needs room for a copy of its project's data and WAL.

A join that stopped after its certificate was issued continues with `sudo supavise node join --resume`. A node that stays `joining` for an hour is removed by the leader. A server that holds the identity of a join that was given up, of a node that was removed while it was down, or of one that cannot rejoin, starts over with `sudo supavise node join --reset --token-file F` and a new token. `--reset` lists the data it sets aside, asks, and refuses a server that leads its cluster unless you pass `--yes`.

To remove a server, run `sudo -u supavise supavise node rm <node>` on the leader. It removes the node's replicas and revokes its certificate, and the node retires itself: it stops what runs, sets its data aside and waits to be joined again. A node that is the home of a project is refused: move its projects first.

**On AWS.** Make the second server as a second stack from the same template, in another zone or another region. The script reads the leader stack's outputs, stores the token as a Secrets Manager secret in the new region, creates the stack with `JoinLeader` set, opens the leader's security group to the new Elastic IP, and waits:

```bash
(umask 077; sudo -u supavise supavise node token --region eu-west-1 > token.txt)    # on the leader's node; copy the file to where you run the script
./supavise-aws-deploy.sh replica --leader-stack supavise --region eu-west-1 --az eu-west-1b --token-file token.txt
```

The leader's stack must be at infrastructure revision 2: run [`update`](#bring-a-v01x-aws-stack-forward) first. The new stack installs the release of the template the script used, not the leader's version, so upgrade the leader first when the two differ. `deploy/aws/README.md` lists the steps and what stays behind when the script stops. Each stack's security group needs the other's Elastic IP on 7443 (`PeerCidr1`); the script sets both. Give independent deployments in one AWS account their own `ClusterName` ([What the stack creates](#what-the-stack-creates)).

A server of a cluster may call `ec2:DescribeInstances`, the one read that the mesh resolver makes when a peer's address stops answering. `FencingPolicy` includes it with `Failover=on`, and `ClusterDescribePolicy` gives that single action to a stack that has a `PeerCidr` or `JoinLeader`, so it works without automatic failover. A single server gets neither.

## Failover and failback

Run `supavise failover` and `supavise projects failover <ref>` with `--dry-run` first. Each prints every precondition with its verdict and changes nothing. [The reference](../docs/reference/replicas.md#failover) has the step lists, the failure matrix and the bounds on data loss.

**Switchover** (the old primary is alive: nothing is lost).

```bash
sudo -u supavise supavise projects failover <ref> [--to NODE]   # on the leader: one project
sudo -u supavise supavise failover [--to NODE]                  # on the node that takes over: the whole server
```

A project move runs on the leader, shows `RESTARTING`, and the old primary becomes a replica in place. A server move quiesces the leader (maintenance, project clusters stopped eight at a time, then the system cluster), takes the service address, promotes the standby of the registry and moves each project four at a time. Each node's daemon restarts once when its system cluster changes role, so a server move shows a short restart at each node, and the CLI waits for it. A move that stopped continues with `--resume`. A failed server move that went no further than stopping the leader is discarded with `--abort`.

**Failover** (the old primary is dead or silent: at most the replica's lag is lost). Run the same commands on a survivor. A project failover asks you to type the project's ref before it fences the old primary and sets its data aside (`--yes` skips the question). The old primary must be fenced before anything is promoted:

| `[failover] fencing` | What happens |
|---|---|
| `aws` | The survivor stops the peer's EC2 instance, waits for `stopped` (up to `stop_timeout_seconds`, forcing once), and associates the service address's Elastic IP with itself. A peer that EC2 does not list is not taken for stopped. A survivor whose only private address carries an Elastic IP of its own keeps it; the move prints the DNS change instead. |
| `command` | `fence_command` runs on the survivor through `/bin/sh` and must exit 0 before any promotion; `takeover_command` then moves the address. They get `OLD_NODE`, `NEW_NODE`, `OLD_NODE_ID`, `NEW_NODE_ID`, `OLD_NODE_ADDR`, `NEW_NODE_ADDR`, `EPOCH` and `PLANNED` in a short environment, not the daemon's. |
| empty | Nothing is fenced. `--old-primary-is-down` states that the old leader cannot write; the survivor still tries the cooperative fence. The move prints the address or DNS change for you to make. |

Preconditions that a move enforces: every replica needed is a healthy standby with lag under `[failover] max_lag_seconds` (30), both nodes run the same release, the backup store is reachable for the epoch marker, and a server move has a replica for every project (or `--restore-missing`, which seeds a standby from the WAL archive and loses up to `archive_timeout` of that project's writes) and Storage on S3. `--force` overrides the ones that are not marked hard.

**Automatic failover** is off. `[failover] mode = "project"` fails over one project whose primary stays unhealthy for `project_grace_seconds`; `"server"` also lets a follower take over a dead leader. Both need `fencing = "aws"`, an S3 backup store and a probe that passes: the node asks EC2 with `DryRun` whether its role may stop the peer and move the address, and when the probe fails it runs as manual and raises `failover_auto_off`. On AWS, turn the permissions on with `sudo -E supavise upgrade --aws --set Failover=on`, or `update --set Failover=on` on each stack; the permissions cover only resources that carry the stack's `supavise:cluster` tag. Server mode acts only when the leader stops answering pings for `grace_seconds`, the health probe of the service address fails and EC2 reports the leader stopped, terminated or failing its status checks. No automatic move runs during an announced maintenance, within `cooldown_minutes` of another failover, while the nodes run different releases, across regions, or when a replica's lag is unknown or over the limit. A project with no replica makes the whole server move refuse, so automatic server mode waits for you.

**The old primary returns.** A node that led and comes back asks its peers for the epoch and reads `_node/leader.json` from the backup store. A higher epoch fences it: no database starts, every host answers `503`, `supavise status` shows FENCED and the critical alert `fenced` fires. Run `sudo supavise node rejoin` on it. It moves the old data to `data.diverged-<epoch>` (kept `keep_diverged_days`, 3, then removed), builds a fresh standby of the registry from the leader's archive, keeps its identity and rebuilds its replicas. After a planned switchover the old primary follows and needs no rejoin.

**Failback** is a switchover back. Once the returned node is a follower and its replicas are healthy, run `supavise failover` on it (or `supavise projects failover <ref> --to NODE`). Nothing moves back by itself.

**AWS notes.**

- *Where the service address sits.* After a failover the Elastic IP is associated with the survivor's instance, and the leader's stack still lists its own association. A stack update that touches the instance or the association could move the address back. `supavise-aws-deploy.sh update` reads where the address is (`describe-addresses`) before it shows the change set and again before it applies it, and refuses to run a change set that would move it or update the instance while the address is elsewhere: move the address back by failing back, then update. Whether a tags-only change moves the address is for `rehearse.sh` to show.
- *Legacy stacks.* A v0.1.x stack has one Elastic IP and no secondary private address. When it is the node that loses the service address it keeps only an auto-assigned public address. The survivor reaches it through the mesh, and failback restores its address. A replica server stack has a second private address for the takeover.
- *Not run in AWS.* FencingPolicy names no network interface, and a replica server takes the address on its secondary private IP, so `associate-address --private-ip-address` may be denied. `rehearse.sh` prints the dry-run command for it. Read the answer before you rely on a failover onto a replica server.
- *Cross-region.* An Elastic IP belongs to one region, so a failover across regions is manual and the move prints the DNS change.

## AWS

One CloudFormation template (`cloudformation/supavise.yaml`) creates a complete node from an admin email; everything else has a default. Every release attaches it as `supavise.yaml` (with that release as its default) and the script of path b as `supavise-aws-deploy.sh`. The three ways to deploy give the same stack.

### a. Console upload

Download `supavise.yaml` from the [latest release](https://github.com/supavise/supavise/releases/latest). In the AWS console, pick your region, open CloudFormation, then **Create stack, With new resources, Upload a template file**. Enter a stack name (`supavise`) and your **Admin email**, leave the rest as it is, tick **I acknowledge that AWS CloudFormation might create IAM resources**, and create the stack. About ten minutes later it reaches `CREATE_COMPLETE`; open the **Outputs** tab ([First login](#first-login)).

### b. deploy.sh

```bash
curl -fsSLO https://github.com/supavise/supavise/releases/latest/download/supavise-aws-deploy.sh
chmod +x supavise-aws-deploy.sh
./supavise-aws-deploy.sh --region us-east-1 --email you@example.com
./supavise-aws-deploy.sh --region us-east-1 --email you@example.com \
  --domain example.com --hosted-zone-id Z0123456789ABCDEFGHIJ     # your own domain in Route 53
```

The script runs `aws cloudformation deploy`, waits, then prints the dashboard URL and the command that fetches the claim token. It needs the AWS CLI v2 and credentials that may create CloudFormation, EC2, IAM, S3, Route 53 and Secrets Manager resources. `--help` lists the options (`--instance-type`, `--stack-name`, `--volume-size`, `--daily-snapshots`, `--version`, `--access-cidr`, `--ssh-cidr` with `--key-name`, `--no-session-manager`, `--ami-id`, `--data-snapshot-id`, `--profile`). `--dry-run` prints the exact `aws` commands, including the lookups, and runs none. Running it again with the same `--stack-name` updates the stack, passing the image the instance already runs so a newer Ubuntu image does not replace the instance. `--delete` stops the instance, then deletes the stack, after a confirmation ([Tear down](#tear-down)). From a checkout it is `deploy/aws/deploy.sh`.

### c. Launch Stack button

CloudFormation reads a template from S3 only, so the button needs a public bucket that the maintainers own; until they set one up, releases carry no button. The repository owner sets it up once with their own AWS credentials (nothing in this repository creates AWS resources):

```bash
BUCKET=supavise-templates-CHANGE-ME REGION=us-east-1     # the bucket name must be globally unique
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
aws s3api create-bucket --bucket "$BUCKET" --region "$REGION" \
  $([ "$REGION" = us-east-1 ] || echo --create-bucket-configuration LocationConstraint="$REGION")
aws s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=false,RestrictPublicBuckets=false
aws s3api put-bucket-policy --bucket "$BUCKET" --policy '{"Version":"2012-10-17","Statement":[{"Sid":"PublicReadTemplates","Effect":"Allow",
  "Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::'"$BUCKET"'/templates/*"}]}'
aws iam create-open-id-connect-provider --url https://token.actions.githubusercontent.com \
  --client-id-list sts.amazonaws.com    # once per account; skip it if `aws iam list-open-id-connect-providers` shows one
aws iam create-role --role-name supavise-release-templates --assume-role-policy-document '{
  "Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sts:AssumeRoleWithWebIdentity",
  "Principal":{"Federated":"arn:aws:iam::'"$ACCOUNT"':oidc-provider/token.actions.githubusercontent.com"},
  "Condition":{"StringEquals":{"token.actions.githubusercontent.com:aud":"sts.amazonaws.com",
    "token.actions.githubusercontent.com:sub":"repo:supavise/supavise:environment:release"},
  "StringLike":{"token.actions.githubusercontent.com:job_workflow_ref":
    "supavise/supavise/.github/workflows/release.yml@refs/tags/v*"}}}]}'
aws iam put-role-policy --role-name supavise-release-templates --policy-name put-templates \
  --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject",
  "Resource":"arn:aws:s3:::'"$BUCKET"'/templates/*"}]}'
gh variable set AWS_TEMPLATE_BUCKET --repo supavise/supavise --body "$BUCKET"
gh variable set AWS_TEMPLATE_REGION --repo supavise/supavise --body "$REGION"
gh variable set AWS_RELEASE_ROLE_ARN --repo supavise/supavise --body "arn:aws:iam::$ACCOUNT:role/supavise-release-templates"
```

If the public access block fails with access denied, the account has Block Public Access on for all buckets; turn off only its "block public policy" and "restrict public buckets" settings, or skip the button. The role trusts one token, the one the `release` job of `release.yml` gets for the GitHub Environment `release` on a `v*` tag, and cannot read, list, delete or write outside `templates/`. The `publish-template` job then uploads `templates/supavise-<tag>.yaml` (and, for a stable tag, `templates/supavise-latest.yaml`), checks that an anonymous download returns the same bytes, and prints the Launch Stack link (it creates the stack in the bucket's region) in the job log and run summary. The link has this shape, with the template URL URL-encoded:

```
https://<region>.console.aws.amazon.com/cloudformation/home?region=<region>#/stacks/create/review?templateURL=<url-encoded https://BUCKET.s3.REGION.amazonaws.com/templates/supavise-latest.yaml>&stackName=supavise
```

### First login

The stack's **Outputs** include `DashboardUrl` (Studio), `ClaimUrl`, `ClaimTokenCommand` (an `aws secretsmanager get-secret-value` command that prints the one-time claim token), `ApiUrl` (Supabase CLI `--profile`, MCP `--api-url`), `BackupBucket`, `InstanceId` and `ConnectCommand` (a Session Manager shell), `DataVolumeId`, `DataSnapshotsCommand` (lists the volume's daily snapshots; absent when `DailySnapshotsKept` is 0), and `PublicIp` with `DnsRecordsNeeded`. Run the `ClaimTokenCommand` value, open `ClaimUrl`, enter the token, an email and a password, then sign in at `DashboardUrl`. The token works once and expires after 72 hours; while nobody has claimed, `sudo -u supavise supavise claim token` in a shell issues a new one. The first HTTPS request to a new host name waits a few seconds while the node gets its certificate.

### Domain and DNS

- **Neither domain nor zone (the default).** The node uses `studio.<ip>.sslip.io`, `api.<ip>.sslip.io` and `<ref>.api.<ip>.sslip.io`, where `<ip>` is the Elastic IP; [sslip.io](https://sslip.io) resolves such names to the IP inside them. Each host gets its own Let's Encrypt certificate (HTTP-01) on first use, so port 80 must stay open. Use it for a trial: Let's Encrypt limits certificates per registered domain per week, and every project adds a host.
- **A domain in Route 53 (`DomainName` and `HostedZoneId`).** The stack creates the four DNS records and the node gets one wildcard certificate by DNS-01 with the instance role, which can change only `_acme-challenge` TXT records of that zone. The zone must not already hold those four names, or the stack fails and rolls back.
- **A domain elsewhere (`DomainName` only).** Create the four records as `A` records pointing to the `PublicIp` output (`DnsRecordsNeeded` prints them), early: each host's certificate is requested on its first visit and needs the name to resolve.

### Sizing

A node needs about 1.5 GB of memory before the first project and about 150 MB for each idle project (measured, [footprint](../docs/reference/footprint.md)). The default instance, `t4g.large` (Graviton, 8 GiB), holds about 20 projects and leaves about 3.5 GiB for load. The figures keep 1 GiB for the operating system and count idle projects only; a project that serves traffic needs more, so size up for busy projects. The measurement reached 50 projects; the larger rows extend it. A second server holds the replicas you ask for and the projects you move to it, so size it for those: with `[replicas] default = "all"` every project has a replica there, and the server needs about what the first one has ([footprint](../docs/reference/footprint.md#follower-profile)).

| RAM | Instance types | Idle projects that fit |
|---|---|---|
| 4 GiB | `t4g.medium`, `t3.medium` | about 10 (a trial) |
| 8 GiB | `t4g.large` (default), `m7g.large`, `t3.large`, `m7i.large` | about 35; the default aims at about 20 |
| 16 GiB | `t4g.xlarge`, `m7g.xlarge`, `r7g.large`, `t3.xlarge`, `m7i.xlarge`, `r7i.large` | about 90 |
| 32 GiB | `m7g.2xlarge`, `r7g.xlarge`, `m7i.2xlarge`, `r7i.xlarge` | about 200 (beyond the measured 50) |

The `t` types are burstable and slow down when their CPU credits run out, so choose `m7g`/`m7i` or larger for sustained load. Graviton (`g`) types run the arm64 build and x86 types the amd64 build. The data volume (100 GiB by default) holds about 75 MB per new project plus what the projects store. It can grow but never shrink: raise `DataVolumeSize`, update the stack, then run `sudo xfs_growfs /var/lib/supavise` on the instance.

### Project sizes and disk

Each project has a compute size, as on hosted: Nano, Micro, Small, Medium, Large, XL, 2XL and up to 16XL. New projects are Micro (1 GB memory cap, 60 connections). A size sets the memory cap and CPU quota of the project's units, its Postgres settings and its Supavisor pool; the table is in [internal/lifecycle/README.md](../internal/lifecycle/README.md#compute-sizes). Change it in Studio under Project settings, Compute and Disk, with `supavise projects resize <ref> --size small`, or with the Management API (`PATCH /v1/projects/{ref}/billing/addons`). The project restarts (about as long as a pause and a resume) and shows `RESIZING`; if the new size does not start, the old one is put back. A resize is refused, with the setting named, when the project's saved Postgres or pooler settings do not fit the new size (for example a `shared_buffers` of 3GB on a Micro). Prices are always 0. `supavise projects sizes` lists the sizes and which the node can give now.

A size is a memory cap, not a reservation. The node accepts sizes while the caps of its projects add up to its memory times `[compute] overcommit` (default 6, because an idle project uses about 150 MB of a 1 GB cap): 8 GiB gives room for 48 Micro, 24 Small or 12 Medium projects, and the counts double with each doubling of memory. Beyond that it refuses, with the reason, a create with a size, a branch, a restore as a new project, a resize and the resume of a paused project, and any size that needs more cores than the node has; a create without a size takes Micro and is not refused. If projects are busy, lower the ratio: at 1 the caps fit in the node's memory. `supavise status` shows how much is promised. The settings are `[compute] overcommit` (or `SUPAVISE_COMPUTE_OVERCOMMIT`), and `node_memory = "16G"` and `node_cpus = 4` for when `/proc` shows the host's resources and not your VM's.

**Disk size per project.** A project can have a disk size of its own only where the data volume is XFS mounted with `prjquota` (the CloudFormation template mounts it so). Supavise then sets an XFS project quota on the project's directory, which covers its data directory including WAL but not Storage objects, and a write past it fails with "No space left on device" for that project only. The size cannot be set below what the project holds plus 20%. Elsewhere, the size shown is the whole volume, it is informational, and a request to change it is refused with a message that says so. To turn quotas on for an existing XFS volume, add `prjquota` to its `/etc/fstab` line and mount it again (`systemctl stop supavise.service 'supavise-*'`, `umount /var/lib/supavise`, `mount /var/lib/supavise`; a remount cannot switch quotas on) and check `findmnt -no OPTIONS /var/lib/supavise`. The daemon sets the quota through a one-shot unit, `supavise-diskquota@<ref>.service`, because it needs root. IOPS, throughput and disk type are the volume's, and disk autoscaling is not available: grow the volume itself (`DataVolumeSize`, then `xfs_growfs`).

### What it costs

You pay AWS directly. Prices change by region and over time, so check the pages:

- The instance, billed per hour or second ([EC2 on-demand pricing](https://aws.amazon.com/ec2/pricing/on-demand/)). This is the largest part.
- Storage: the 30 GiB root volume, the data volume and the snapshots (the daily ones while the stack runs, the final one after a delete; [EBS pricing](https://aws.amazon.com/ebs/pricing/)). A snapshot stores only the blocks that changed since the one before it; `DailySnapshotsKept` set to 0 takes none.
- The backup bucket and, since revision 2, the objects bucket: stored data, requests and versions kept for 30 days ([S3 pricing](https://aws.amazon.com/s3/pricing/)).
- A second server is a second stack with its own instance, data volume, snapshots and Elastic IP. Replication also moves WAL between the servers, which AWS bills as data transfer when they sit in different zones or regions ([EC2 on-demand pricing](https://aws.amazon.com/ec2/pricing/on-demand/)).
- Data transfer out of AWS ([EC2 on-demand pricing](https://aws.amazon.com/ec2/pricing/on-demand/)), the public IPv4 address of the Elastic IP ([VPC pricing](https://aws.amazon.com/vpc/pricing/)), one Secrets Manager secret ([pricing](https://aws.amazon.com/secrets-manager/pricing/)) and, with a hosted zone, Route 53 ([pricing](https://aws.amazon.com/route53/pricing/)).

### What the stack creates

One Ubuntu 24.04 instance (Graviton by default), an Elastic IP, a data volume formatted XFS and mounted at `/var/lib/supavise` with `prjquota` (XFS with reflinks is what copy-on-write branching needs), an S3 bucket for backups (versioned, encrypted, public access blocked, TLS only), a second bucket for Storage's objects and a `StorageRole` scoped to it, tags on the instance that let the node find its stack, a security group for ports 80, 443, 5432 and 6543 (plus port 7443 from the ranges you list in `PeerCidr1` to `PeerCidr3`), an instance role, a Secrets Manager secret for the claim token (and, with `KeyEscrowPassphrase`, one for the passphrase), and a Data Lifecycle Manager policy with its own role that snapshots the data volume daily. Without a VPC of your own it also creates a small VPC with one public subnet. Everything sits in one file (no nested stacks, Lambda code or custom resources), so the console can upload it as it is.

The console form describes each parameter; `AdminEmail` is the only required one. Worth knowing: `InstanceType` (default `t4g.large`, see [Sizing](#sizing)), `DataVolumeSize` (GiB, default 100), `DailySnapshotsKept` (0 to 1000, default 7; `0` creates no policy), `SupaviseVersion` (`latest` or a tag; a release's template names that release), `DomainName` and `HostedZoneId` ([Domain and DNS](#domain-and-dns)), `VpcId` and `SubnetId` (an existing VPC and public subnet, both or neither), `DataSnapshotId` (restore the volume from an earlier stack's snapshot, only when creating a stack; see [Tear down](#tear-down)), `AccessCidr` (who may reach the four public ports; default everyone, and port 80 must stay open for HTTP-01 without a hosted zone), `SshCidr` and `KeyName` (optional SSH as `ubuntu`, off by default), and:

- `ClusterName` (the stack name by default) is the value of the `supavise:cluster` tag that ties the servers of one cluster together: the fencing permissions cover only instances and addresses that carry it, and the Storage role admits only roles that carry it. Stack names repeat across regions, so give each independent deployment in one account its own `ClusterName`, or a server with `Failover` on could stop another deployment's instance. `Failover` (`off` by default) adds the fencing permissions. `PeerCidr1` to `PeerCidr3` open port 7443 to the second server. `JoinLeader`, `JoinTokenSecretArn`, `BackupBucketName`, `BackupBucketRegion`, `ObjectsBucketName` and `StorageRoleArn` make the stack a replica server ([Add a replica server](#add-a-replica-server)); `supavise-aws-deploy.sh replica` sets them.
- `EnableSessionManager` (`true` by default) gives a shell with `aws ssm start-session` and no inbound port. The role gets only the Session Manager channel actions, not `AmazonSSMManagedInstanceCore`, which can read every Parameter Store parameter in the account.
- `AmiId` is empty on the first launch (the current Canonical Ubuntu 24.04 image). On later updates, give the image the instance runs ([Updating the stack](#updating-the-stack)).
- `KeyEscrowPassphrase` (optional, hidden with `NoEcho`, 12+ characters) makes the installer keep a copy of the master key and `config.toml` in the backup bucket, encrypted with it. Set it when you create the stack and keep it on later updates.

The instance role may write only this bucket, put a value into only the claim-token secret (and, with `KeyEscrowPassphrase`, read and replace only the passphrase secret), change only TXT records named `_acme-challenge.<domain>` or `_acme-challenge.*.<domain>` in the given hosted zone, and, unless `EnableSessionManager` is `false`, use Session Manager. Since revision 2 it may also assume exactly one other role, `StorageRole`, and, for a stack that is part of a cluster (`PeerCidr1` or `JoinLeader` set), `ec2:DescribeInstances`, one read that takes no resource. With `Failover` on it may also describe instance status and addresses, and stop instances and move addresses, only for resources that carry the stack's `supavise:cluster` tag. A replica server's role may also read the one secret that holds its join token. A process on the instance could take the role from the instance metadata service; the tenant-facing units cannot reach it (`deploy/systemd/README.md`): every `supavise-*` unit denies it with `IPAddressDeny`, WAL archiving goes through the daemon (`internal/backup/README.md`, "The WAL relay"), and Storage's S3 backend takes the credentials of a role that the daemon assumes for it (`StorageRole`). Only `supavise.service` and the base backup units use the instance role. 

User data prepares the volume, runs `install.sh` from the release with the stack's parameters, stores the claim token in the secret and signals the stack with `cfn-signal` (with a `curl` fallback). The stack waits 30 minutes for the signal and otherwise fails with the installer's last error; the installer log is `/var/log/supavise-bootstrap.log`. User data bind-mounts `/etc/supavise` (the master key and `config.toml`) to `/var/lib/supavise/etc` first, with an `fstab` entry and `RequiresMountsFor=` on the daemon and the system Postgres, so the data volume holds everything the node needs to come back and nothing needed to restart sits on the root volume. When the volume already holds an install, user data recreates the `supavise` user with the uid that owns the files and the installer runs as a repair that keeps the master key, the registry, `config.toml` and the claim token. If a create fails, CloudFormation rolls back and deletes what it made except what is retained: an empty backup bucket and a snapshot of the empty data volume may remain; delete them. To keep the instance for debugging, choose **Preserve successfully provisioned resources** under **Stack failure options** in the console.

**Storage on S3.** From revision 2 the stack creates the objects bucket (`ObjectsBucket`: versioned, encrypted, public access blocked, kept when the stack is deleted) and `StorageRole`, a role that only the stack's instance role may assume, scoped to that bucket; no access key exists. The stack does not switch Storage to S3, because that moves objects. Take the two values from the stack's outputs (`aws cloudformation describe-stacks --stack-name <name> --query 'Stacks[0].Outputs'`), set `storage_s3_region` under `[fleet]` in `/etc/supavise/config.toml` to the stack's region, and run on the node `sudo -u supavise supavise storage migrate --to s3 --bucket <ObjectsBucket> --role-arn <StorageRoleArn>`. The command copies the objects, switches Storage (`storage_backend` and the bucket go to `config.toml`) and writes `storage_s3_role_arn` to `config.d/30-storage-s3.toml`, never a key. The `[fleet] storage_*` settings belong to the cluster: a replica server of the stack (`JoinLeader`) takes them from the leader at its next `supavise system converge`, which an upgrade runs, so run the command on the leader only. The installer does not read the `supavise:storage-role` tag or write these settings: the first boot of a new stack leaves Storage on files until the command runs.

### Backups and restore

Every project archives its WAL to the backup bucket and takes a nightly base backup; the same nightly run copies its Storage objects and Edge Function deployments (only what changed). The daemon prunes old ones (`backup.retention_days`, 7 by default). The [guide](../docs/guide.md#back-up-and-restore) covers restoring from the dashboard and with `supavise backups` (on AWS, `aws ssm start-session` gives a shell). The database comes back to the second you ask for; Storage objects and functions come back to the newest nightly copy at or before it. A restore refuses to start when the disk lacks room for a second copy of the project's data, and the previous data directory stays next to the new one (`projects/<ref>/postgres/data.pre-restore-<time>`) until the project's next restore that works. Do not restart `supavise` while a project shows RESTORING; a restart that cuts the restore off leaves it RESTORING until you settle it by hand (`internal/backup/README.md`, "From the dashboard and the Management API").

**The bucket alone cannot rebuild a node.** The passwords in a backup are sealed with the node's master key, which is not in the bucket in the clear. Keep it with `supavise system export-key` (offline) or as an encrypted copy in the bucket (`system escrow-key`, `system restore-key`; `internal/backup/README.md`). On AWS, `KeyEscrowPassphrase` makes the copy at the first boot: the stack puts the passphrase in a Secrets Manager secret (user data never holds it), the instance reads it into a root-only file for `--key-passphrase-file`, then overwrites the secret with a note and removes the file. The passphrase also sits in the stack's parameters (`NoEcho` hides it in the console and `describe-stacks`); keep your own offline copy, since neither the node nor the secret keeps it afterwards. An update that leaves the parameter empty removes the secret, and a changed value does not re-encrypt an existing copy (run `system escrow-key`). `deploy.sh` has no option for it; use the console or `aws cloudformation create-stack`. To rebuild a node, run `system restore-key` before `supavise install`, so the installer keeps the old key; if the node already installed with a new key, `system restore-key --key-id <old key id> --force` puts the old one back before you restore any project.

**Daily snapshots of the data volume.** The bucket lacks `config.toml` and the master key (unless you made the escrow), and objects stored since the last nightly copy live on the data volume only. A Data Lifecycle Manager policy therefore snapshots the data volume daily at 03:00 UTC and keeps the newest `DailySnapshotsKept`. It selects the volume by a tag whose value is the stack's ID, so it cannot reach another stack's volume; each snapshot carries the `Name` tag (`<stack name>-data`) and `supavise:snapshot=daily`. Its role carries the AWS managed policy `AWSDataLifecycleManagerServiceRole`, which is wider than this one volume (it can create and delete snapshots in the account); the template uses it because a hand-written subset that misses an action would stop the snapshots without a visible error. The snapshots are crash-consistent, so a file a service was writing at that moment can be incomplete; the S3 point-in-time backups stay the primary database backup, and the snapshots protect the rest of the volume and give a whole-node fallback. To restore from one, list them (`DataSnapshotsCommand`, or `aws ec2 describe-snapshots --owner-ids self --filters Name=tag:Name,Values=<stack name>-data`) and create a new stack with `DataSnapshotId` set to the one you pick, as in [Bring the node back](#tear-down). A volume made from a snapshot cannot replace the volume of a running stack, so use a new stack name while the old stack exists, or delete it first. The whole node returns to the state of that snapshot; for one project at a point in time, use `supavise backups restore`.

### Bring a v0.1.x AWS stack forward

A stack that v0.1.x made is at infrastructure revision 1. The template of this release is at revision 2. The update adds resources and permissions, changes tags and one metadata option, and replaces or interrupts nothing: a test renders the v0.1.1 template and the current one with the v0.1.1 parameters and fails when any property of the instance, the data volume, the address or the network that would replace or interrupt it differs. `supavise status` shows the gap:

```
Infrastructure  AWS stack "supavise" is at revision 1; this release needs 2 for: replicas, S3 Storage, failover
  missing  Instance tags readable from the node   (needed to find the stack from the node)
  missing  Storage bucket and role                (needed for S3 Storage and server failover)
  ...
  Fix: sudo -E supavise upgrade --aws
```

**What revision 2 adds.**

- Tags on the instance (`supavise:cluster`, `supavise:stack-name`, `supavise:infra`, `supavise:eip`, `supavise:storage-role`, `supavise:caps`) and `InstanceMetadataTags`, so the node reads its stack's name and revision from the metadata service with no IAM permission.
- `ObjectsBucket` (versioned, encrypted, TLS only, kept when the stack is deleted) and `StorageRole`, which is scoped to that bucket. The instance role may assume exactly that role. No access key or stored secret exists.
- Off until you ask: `PeerCidr1` to `PeerCidr3` (a rule for port 7443 from that range, which also adds `ClusterDescribePolicy`, the single read `ec2:DescribeInstances`), `Failover` (`on` adds the fencing permissions) and `ClusterName` (the value of the `supavise:cluster` tag).
- Outputs `ElasticIpAllocationId`, `ObjectsBucket`, `StorageRoleArn`, `SecurityGroupId`, `InfraRevision` and `ClusterName`.

The update does not change what Storage runs on. Storage keeps its files until you run `sudo -u supavise supavise storage migrate --to s3 --bucket <ObjectsBucket> --role-arn <StorageRoleArn>` ([Storage on S3](#storage-on-s3)).

**Steps.**

1. **Rehearse.** From a checkout of the release's tag (releases do not attach this script), run `deploy/aws/rehearse.sh --region us-east-1 --email you@example.com`. It makes a throwaway stack from the v0.1.1 template, updates it to this release's template with the failover permissions and a peer rule on, checks that the instance has the same id and launch time and had no create or delete event, that a second update changes nothing, and deletes the stack. It costs money while it runs. It also prints what to check on the node. No run of it is recorded, so this is the check against AWS.
2. **Land the new binary.** On the node, run `sudo supavise upgrade`. A v0.1.x binary drives this first step ([the first hop](#upgrading-the-node-supavise-upgrade)); it moves the binary and the host layer.
3. **Update the stack.** With your AWS credentials in the environment, on the node run `sudo -E supavise upgrade --aws`. For a stack made before instance tags, add `--stack-name NAME`; the name is remembered in `/etc/supavise/config.d/20-aws.toml`. Or run the script anywhere, such as CloudShell:

   ```bash
   curl -fsSLO https://github.com/supavise/supavise/releases/latest/download/supavise-aws-deploy.sh
   chmod +x supavise-aws-deploy.sh
   ./supavise-aws-deploy.sh update --stack supavise [--set Failover=on] [--set PeerCidr1=203.0.113.4/32]
   ```

   Without credentials, `supavise upgrade` does the node's side and prints the command to run. The two layers can run in either order, because every stack change is inert until used.
4. **Read the change set and type `apply`.** The script verifies the signature of `SHA256SUMS`, the template and itself, builds a change set with `UsePreviousValue` for every parameter the stack has (so `AdminEmail` and `KeyEscrowPassphrase` need not be given again) and the template's default for each new one, and shows every resource change as allowed, refused or blocked. It adds resources, tags and rules. It refuses any change that would replace or remove a resource other than a security group rule, an IAM policy or a bucket policy, and any in-place change it does not know to be safe, such as `UserData` or `InstanceType`. It blocks one that would update the instance or move the service address back after a failover. A refused or blocked change set is deleted, nothing changes, and the exit status is 2. `--allow-risky` runs a refused one after you type the stack name; a blocked one never runs.
5. **Check.** `supavise status` no longer shows the gap, and `./supavise-aws-deploy.sh status --stack supavise` prints the stack, its revision and whether the service address sits on its instance.

**Operator permissions.** `update`, `status` and `replica` act with your credentials and refuse a credential that is an instance role. The script calls `cloudformation:DescribeStacks`, `CreateChangeSet`, `DescribeChangeSet`, `GetTemplate`, `ExecuteChangeSet` and `DeleteChangeSet`, `ec2:DescribeInstances` and `DescribeAddresses`, and `sts:GetCallerIdentity`. A template over 51,200 bytes goes through S3: it needs `s3:PutObject` and `GetObject` on `s3://<BackupBucket>/_stack/`, because CloudFormation reads the template with your permissions. `replica` also calls `secretsmanager:CreateSecret` and `DeleteSecret`, and may create a template bucket. CloudFormation then makes the changes with your permissions too, unless you pass a service role. For revision 2 that means creating the Storage role and the policies on the instance role (`iam:CreateRole`, `PutRolePolicy`, `DeleteRolePolicy`, `TagRole`, `GetRole`, `GetRolePolicy`), the objects bucket and its policy (`s3:CreateBucket`, `PutBucketVersioning`, `PutEncryptionConfiguration`, `PutBucketPublicAccessBlock`, `PutBucketOwnershipControls`, `PutLifecycleConfiguration`, `PutBucketPolicy`), and the tags, the metadata option and the 7443 rules (`ec2:CreateTags`, `ModifyInstanceMetadataOptions`, `AuthorizeSecurityGroupIngress`, `RevokeSecurityGroupIngress`). That list comes from the template's resource types and the script's calls and has not been tried against AWS. An administrator role, such as CloudShell's, covers it; a narrower policy shows up as an access-denied event in the stack's events. The node's instance role never gains CloudFormation, IAM or EC2-management rights beyond the fencing permissions, and `supavise upgrade --aws` clears the variables that would replace the script's trust root (`SUPAVISE_DEPLOY_PUBKEY_B64`, `SUPAVISE_DEPLOY_BASE_URL`).

**The `SupaviseVersion` repair rule.** `update` never changes `SupaviseVersion`: it feeds the instance's user data, and a different value would replace the instance. Before you replace an instance on purpose (a new image or changed user data does), set `SupaviseVersion` to the release the node runs. A replacement installs `SupaviseVersion` over the data volume, and the value the stack was made with is older than the data on it. `supavise-aws-deploy.sh status` prints this rule.

**Where the address sits.** After a failover the service address belongs to another instance, and `update` blocks any change that would take it back ([Failover and failback](#failover-and-failback)).

### Updating the stack

On the instance, follow the routine in [Update](#update): `supavise upgrade --plan`, then `sudo supavise upgrade`, which moves the binary, the host layer, the services and the projects (`sudo supavise self-update` is the binary-only path). By itself neither touches the stack, which keeps its parameters and template; `sudo -E supavise upgrade --aws` and `supavise-aws-deploy.sh update` bring it to the release's template ([Bring a v0.1.x AWS stack forward](#bring-a-v01x-aws-stack-forward)), and `--set NAME=VALUE` changes a parameter through the same reviewed change set. To change a parameter another way (for example `AccessCidr` or `DataVolumeSize`), run `deploy.sh` again with the new value, or in the console choose **Update, Use existing template** and enter the image ID the instance runs in **AmiId** (EC2 console, instance details) if you left it empty at creation: an empty `AmiId` on an update may make CloudFormation look up the newest Ubuntu image again, and a changed image replaces the instance (this has not been checked in AWS).

Do not change `AmiId`, `InstanceType` to another architecture, the subnet or the root volume of a running stack casually: CloudFormation creates the new instance first and then tries to attach the data volume while the old one still holds it, so the update fails and rolls back. To move a node to a new Ubuntu image, upgrade the OS on the instance (`apt-get dist-upgrade`, then a reboot); `InstanceType` within the same architecture restarts the instance in place. When an instance really has to be replaced, stop the node on it (`sudo systemctl stop supavise.service`, `sudo -u supavise supavise fleet stop`, `sudo -u supavise supavise system stop`, `sync`; the daemon leaves project units running, and stopping them gives a clean data volume), snapshot the data volume and **detach it** (`aws ec2 detach-volume --volume-id <id>`, wait for `available`), then update the stack with the new `AmiId`. The new instance attaches the volume and the node comes back with the same master key, registry, projects and Elastic IP. Then check `sudo -u supavise supavise system status` and `supavise projects list`. This procedure has not run in AWS, so keep the snapshot until it has worked once.

### Tear down

Deleting the stack never deletes your data silently:

| Resource | When the stack is deleted |
|---|---|
| Backup bucket (`BackupBucket`) | **Kept**, with its archives and bucket policy. You pay for it until you delete it. |
| Objects bucket (`ObjectsBucket`, revision 2 and later) | **Kept**, versioned, with whatever Storage keeps in it once Storage runs on S3. |
| Data volume (`DataVolumeId`) | **Snapshotted, then deleted.** The encrypted snapshot holds the master key, the registry and every project. |
| Daily snapshots | **Kept, and no longer pruned**: deleting the lifecycle policy does not delete its snapshots (`aws ec2 delete-snapshot`). |
| Instance, Elastic IP, security group, role, claim-token secret, DNS records, a VPC the stack made | Deleted. |

The stack of a replica server (`JoinLeader` set) makes no buckets: the ones it shows are its leader's, and deleting it leaves them as they are.

Stop the instance before you delete the stack: the final snapshot is taken when the stack deletes the volume, and a snapshot of a running node is only crash-consistent. `./supavise-aws-deploy.sh --region <region> --stack-name supavise --delete` does it: it prints the bucket, volume and instance, asks you to type the stack name, stops the instance, waits until it is stopped, and only then deletes the stack (it keeps the stack if the instance does not stop). A stack whose first launch failed (`ROLLBACK_COMPLETE` or `CREATE_FAILED`) has no instance; the script deletes it without stopping anything. `cloudformation deploy` cannot update a `ROLLBACK_COMPLETE` stack, so deleting it is the way to try again. In the console, stop the instance first, then delete the stack. `aws ec2 describe-snapshots --region <region> --owner-ids self --filters Name=volume-id,Values=<DataVolumeId>` lists the snapshots afterwards.

**Bring the node back.** Create a new stack with `DataSnapshotId` set to that snapshot and `DataVolumeSize` at least the snapshot's size. User data finds the earlier install on the volume and the installer runs as a repair that keeps the master key, the registry and the projects. The new stack makes a new bucket and a new Elastic IP, so update DNS if you manage it yourself; the old bucket still holds the older archives. The existing administrator signs in at the new `DashboardUrl` as before. The repair issues no claim token when the node is already claimed (`ClaimTokenCommand` then prints a note); when it was never claimed, user data leaves the secret as it is, because it can hold the only copy of a live token. When you need a token, run `sudo -u supavise supavise claim token --force` in a shell (without `--force` while nobody has claimed).

**Delete everything.** One command leaves nothing in the account, and none of what it destroys can be brought back:

```bash
./supavise-aws-deploy.sh --region <region> --stack-name supavise --delete --purge
./supavise-aws-deploy.sh purge --region <region> --stack-name supavise     # the stack is already deleted
./supavise-aws-deploy.sh purge --region <region> --stack-name supavise --dry-run
```

`--delete --purge` finds, before it stops anything, the stack's backup and objects buckets (with the number of object versions and delete markers and their size) and the snapshots of the data volume, lists them with the final snapshot that the stack's deletion will take, and asks you once to type the stack name. It then stops the instance, deletes the stack, and destroys everything listed, the final snapshot included. `purge` does the same for a stack that was deleted without `--purge`, refuses while the stack exists, and finds only what carries the stack's own marks: the buckets whose CloudFormation tags name the stack, its id and `BackupBucket` or `ObjectsBucket`, the snapshots tagged with the stack's id, and the other snapshots of the volumes those came from, which is how the final snapshot (it has no tag of its own that the script relies on) is found. A bucket of the account that merely starts with the same letters, such as the template bucket `supavise-templates-<account>-<region>`, another stack's bucket and a snapshot that is neither tagged with the stack's id nor taken from its volume are never touched; a snapshot you took of the volume by hand is the stack's and goes with the others. A bucket that shows no CloudFormation tags (or another stack's) stops `--delete --purge` before anything is stopped. Buckets are emptied with the AWS CLI alone (`list-object-versions` and `delete-objects` in batches of 1000, then the bucket). `--dry-run` prints every `aws` command; `--yes` skips the question. The leader's buckets are the ones its replica servers use: the list names the stacks of the region that use a bucket, and cannot see a replica server in another region, so check that none uses the leader before you confirm. Anything S3 or EC2 refuses (an object under Object Lock, a snapshot an image uses) is reported, the rest goes on, and the script exits 3 so that you run it again after fixing it. A final snapshot that is older than CloudFormation's 90 days of records and has no daily snapshot beside it carries nothing the script can match; delete it by its volume id. The template bucket made for a large template stays: it is shared by the stacks of the region. `deploy/aws/README.md` has the details.

## Release signing

A release is signed by the maintainers' ed25519 key. Set it up once:

```bash
openssl genpkey -algorithm ed25519 -out supavise-signing.pem
openssl pkey -in supavise-signing.pem -pubout -out internal/selfupdate/release_key.pem   # commit this public file
gh secret set SUPAVISE_SIGNING_KEY --env release < supavise-signing.pem                     # then delete supavise-signing.pem
```

The key goes into a GitHub Environment named `release`, not a repository secret: a repository secret can be read by any workflow run that someone with write access starts from any branch, and this key is the root of trust for self-update and `install.sh` on every node. Create the Environment first (Settings, Environments) with a deployment rule that admits only the tags `v*`, and optionally a required reviewer; the first run creates it without a rule.

`.github/workflows/release.yml` runs on a pushed tag `vMAJOR.MINOR.PATCH[-suffix]` (a suffix makes a pre-release):

1. **Gate.** `deploy/release-gate.sh` reads the workflow runs on the tagged commit and goes on only when the newest run (push, manual dispatch or schedule; pull-request runs do not count) of `ci.yml`, `linux.yml`, `conformance.yml` and `replication.yml` succeeded, including the `suites` jobs of `conformance.yml`, the jobs of `linux.yml` whose names start with `upgrade` and the `two-servers` jobs of `replication.yml`. `replication.yml` runs on pushes to `integrate/**` and not on `main`, so dispatch it on the commit to tag (`gh workflow run replication.yml --ref <branch>` runs both architectures; a run takes 35 to 75 minutes). It waits for runs in progress (three hours at most), starts none, and fails when a workflow has no run on the commit (push the commit to `main`, or `gh workflow run <file> --ref <branch>`, then tag). It fails closed: if the upgrade job is renamed, update `REQUIRED` in the script. A failed gate builds and publishes nothing.
2. **Check.** It stops if the committed key is still the placeholder, or the secret is missing or is not the private half of the committed public key; it validates `release_key_next.pem` when it holds a key; then `go vet` and `go test`.
3. **Build and publish.** The amd64 and arm64 binaries and Studio are built; `deploy/release-assets.sh` writes `supavise-release.json` and `SHA256SUMS` (over the binaries, the Studio archives and the manifest), signs the list (`openssl pkeyutl -sign -rawin`, a raw 64-byte signature), stamps the public key into `install.sh`, and copies `supavise.yaml` (the tag as its default `SupaviseVersion`) and `aws/deploy.sh` as `supavise-aws-deploy.sh`, each stamped with the key (and the script with the tag) before the list is made, so that `SHA256SUMS` covers `install.sh`, the template and the script as they are attached; `supavise upgrade --aws` and the script's own `update` check them against it.
4. **Template upload.** When the repository variables `AWS_TEMPLATE_BUCKET` and `AWS_RELEASE_ROLE_ARN` exist, a last job uploads the template to the public bucket and prints the Launch Stack link ([c](#c-launch-stack-button)); otherwise it is skipped.

### The release manifest

`supavise-release.json` is listed in `SHA256SUMS`, so the one signature covers it. `selfupdate.Fetch` (`internal/selfupdate`) refuses a manifest that does not match the signed checksum or whose `version` is not the release tag. Schema 1:

```json
{
  "schema": 1,
  "version": "v1.4.0",
  "min_upgrade_from": "v1.2.0",
  "artifacts": {"auth": "auth-v2.195.0-r1", "postgres": "postgres-17.11.0.004-r1"},
  "studio": "2026.10.05-sha-94b8b06",
  "host": {"converge_revision": 1},
  "aws": {"stack_revision": 2, "template_asset": "supavise.yaml", "template_sha256": "..."},
  "min_peer_from": "v1.3.0",
  "wal_compat": false
}
```

`schema` is the format number: a binary refuses a higher one and says to update with the release's installer; adding a field keeps the number, changing a field's meaning raises it, and readers ignore unknown fields. `version` is the release tag. `artifacts` holds the slim-services release of each Supabase service the release installs, and `studio` the Studio build tag, both from `internal/versions/versions.yaml`. `min_upgrade_from` is the oldest installed version that may upgrade straight to this release; a node on an older version installs it first, a running version that is not a release (`dev`) passes, and a suffix is ignored. It comes from `deploy/MIN_UPGRADE_FROM`: raise it in the commit that makes a release depend on something earlier ones did not do (a registry migration that is not backward compatible, a changed on-disk layout). The other fields are optional and ignored by a reader that predates them. `host.converge_revision` is the host layer revision the release's binary reports in `supavise release-info --json`; `release-assets.sh` runs the binary of the machine's architecture for it (with a minimal environment, because the release job's holds the signing key), and with `SUPAVISE_REQUIRE_CONVERGE=1` a binary that does not report one stops the release, while otherwise the manifest says 0 and the tool warns: set it in the job that signs a release, because a manifest that says 0 weakens the `host_not_converged` gating. `aws` names the stack revision of the attached template, the asset and its SHA-256; the binary's own `infra_revision` has to agree with the template's or no manifest is written. `min_peer_from` is the oldest release a joined server may run beside this one (`deploy/MIN_PEER_FROM`, `SUPAVISE_MIN_PEER_FROM`). `wal_compat` is written only when it is false (`SUPAVISE_WAL_COMPAT=false`): the release's PostgreSQL does not keep the WAL format, and the servers that hold standbys upgrade before the ones that hold the primaries. `deploy/releasetool notes` writes the release notes: the commit subjects since the previous tag (a stable release compares with the previous stable tag, a pre-release with the previous tag of any kind) and a table of the Supabase service versions that moved, each linking the upstream release.

### Rotating the release signing key

Every binary embeds the key that verifies its updates, so a rotation has to reach the nodes through a release that they accept. The binary therefore embeds two keys, `release_key.pem` (current) and `release_key_next.pem` (optional), and accepts a signature by either. `deploy/release-assets.sh` always signs with the current key. A rotation takes two releases and no reinstall:

1. Generate the new pair (`openssl genpkey -algorithm ed25519 -out next.pem`, `openssl pkey -in next.pem -pubout`). Commit the public half as `internal/selfupdate/release_key_next.pem` and keep `next.pem` offline. `release.yml` fails when the file is not a valid public key, is the current key, or when either key file holds a private key or a PEM block that is not a `PUBLIC KEY` (the binary refuses to start on such a next-key file, too).
2. Tag release N. It is signed with the old key, which the nodes trust, and its binary carries both keys. Wait until every node runs N (`supavise update status` on each, or `self-update --check`); nodes in `auto` mode get it in their next window.
3. Put the private half of the new key into the secret (`gh secret set SUPAVISE_SIGNING_KEY --env release < next.pem`), copy the public half over `release_key.pem` and replace `release_key_next.pem` with the placeholder text (or the public half of the following key). Commit and tag release N+1. It is signed with the new key, which N's binaries accept as their next key, and its `install.sh` carries the new key, so new installs trust it from the start.
4. Delete the old private key.

A node that missed release N cannot verify N+1 and must be updated through N first (or reinstalled with the new `install.sh`). A binary built before the second key existed has no overlap and needs the reinstall too. If the old key was stolen, the overlap does not help: whoever holds it can sign a release that nodes accept, so rotate immediately, tell the operators to reinstall with the new `install.sh` from a source they trust, and treat releases signed since the theft as suspect.

### Proposals for new Supabase releases

`.github/workflows/bump-proposals.yml` runs every night, and on demand, on `main`. For each Supabase service (a slim-services artifact, or Studio) whose newest release is newer than its pin in `internal/versions/versions.yaml`, it opens one pull request from `bump/<service>` that moves the pin, or updates the open one (`tests/conformance/bumpcheck`, `tests/conformance/propose-bumps.sh`). It skips pre-releases, a version whose pull request is open or was closed without merging, and a branch that carries a commit by anybody but the bot. **It never merges anything**: it starts `ci.yml`, `conformance.yml` and `linux.yml` (and `studio.yml` for Studio) on the branch with `workflow_dispatch` (a push by `GITHUB_TOKEN` starts none), and their runs gate the merge. It needs the repository setting *Allow GitHub Actions to create and approve pull requests* (Settings, Actions, General). `DRY_RUN=1 tests/conformance/propose-bumps.sh`, or the workflow's `dry_run` input, shows what it would do.

## Tests

The `cfn-lint`, `aws-deploy` and `actionlint` jobs of `ci.yml` check the template, the console form, the IAM scoping, `deploy.sh` against a stub `aws` and the workflows; the `install-e2e`, `upgrade-e2e`, `upgrade-smoke`, `os-updates`, `converge-e2e` and `firstboot-e2e` jobs of `linux.yml` run the install, the claim flow, self-update, the node upgrade and rollback, `supavise system converge` on a node a v0.1.x-shaped release installed, the AWS first boot on loop devices, automatic upgrades through `supavise-upgrade.service`, project upgrades and the OS updates on real machines, and the `cluster-identity`, `replica-blocks`, `fleet-follower` and `storage-migrate` jobs run the cluster certificate authority and join, replica seeding, streaming and promotion on real clusters, a follower's shared services, and the Storage migration against Garage (`tests/linux/README.md` has one row per script); the `pebble` job runs a custom hostname from initialize to activation against a test certificate authority. Nothing here has run in an AWS account. `docs/development.md` describes the workflows.

## Limits

- **No `supavise uninstall`.** Stop and disable `supavise.service` and the `supavise-*` units, then remove `/var/lib/supavise`, `/etc/supavise` and the `supavise` user by hand.
- **A node upgrade restarts the daemon.** HTTPS and the Management API fail for a few seconds and WAL archiving pauses meanwhile; there is no socket hand-over.
- **Custom hostnames** have not run against a real certificate authority and real DNS (CI uses Pebble and a DNS stub). There is no wildcard custom hostname and no CAA check, and certificate-order limits are counted in memory per project and per server, not per organization.
- **`install.sh` resolves `latest` through a redirect of github.com** and trusts TLS for that step only. The tag it gets is then used for signed files, so a wrong tag can only pick an older signed release.
- **The claim page is at `api.<domain>/claim`**, not `studio.<domain>/claim`: the Studio host belongs to Studio.
- **No email** unless `[mail]` is configured: invites and the claim token are handed over out of band.
- **The AWS stack is unverified in AWS.** The update path (`update`, its change-set review and `rehearse.sh`), a replica server stack, the fencing permissions (among them `associate-address` to a secondary private address, which `FencingPolicy` may not allow), instance tags that appear in the metadata service without a restart, and role-based Storage credentials against real session tokens have not run against AWS either. The first launch is the first test of the data-volume discovery, the restore from `DataSnapshotId`, the snapshot policy and its role, the `/snap/bin` fallback for the AWS CLI, the Secrets Manager and key escrow steps (user data retries the read of the passphrase secret for a minute because of IAM delay), the signal, the Session Manager permissions (an SSM agent that needs more than the `ssm` and `ssmmessages` actions shows up as an instance that never appears in Session Manager), the Ubuntu image lookup, the Launch Stack link (which needs the one-time setup in [c](#c-launch-stack-button)) and the template upload.
- **IMDS is reachable from processes outside the `supavise-*` units** (the daemon and the base backup units use the instance role, as intended). A process that gets code execution as the `supavise` user inside a unit can still reach the daemon's and other units' memory through `/proc` (`deploy/systemd/README.md`, "What is not isolated"). The edge runtime must keep `IPAddressDeny`.
- **Ubuntu 22.04 is not supported** (polkit 0.105 ignores JavaScript rules). A sudoers drop-in for `systemctl start|stop|restart|enable|disable supavise-*` would bring it back at the cost of a `sudo` call in the supervisor.
- **No second server on real machines in the deploy guide's path.** The mesh, joining, replicas and failover run in unit tests, on real Postgres clusters in one process, and on the follower and storage jobs of `tests/linux/README.md`. A change set against a real stack, a replica stack and a fencing call need `rehearse.sh` in an account. No recorded run measures how much data a failover loses or how long it takes (`docs/reference/replicas.md`).
- **A project homed on a follower has no failover while its node is down.** A project move needs the home to answer, and a server move moves only the projects of the old leader.
- **A second server needs an S3-compatible backup bucket**, and a failover of the whole server needs Storage on S3.
- **`supavise projects delete` and `supavise backups restore` on the server do not remove a project's replicas first**; the Management API does. Run `supavise replicas rm` first.
- **Not run in CI:** `supavise upgrade --include-postgres` runs in unit tests with a fake node only. `upgrade-e2e` runs `--unattended` end to end once, through the real `supavise-upgrade.service`, between two builds of the same source (a release that moves no service). The OS reboot, and the host lock held across it, are tested with a fake clock, lock and reboot: no CI job reboots a machine. No CI job installs a release's Studio build.
