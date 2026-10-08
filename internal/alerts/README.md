# internal/alerts

Tells the operator when the node needs them. It sends JSON webhooks (optionally signed) and email through the node's `[mail]` relay, sends a problem once instead of every minute, caps what it sends per hour, and runs a checker in the daemon that turns `internal/health` reports into alerts.

```go
alerts.Notify(ctx, alerts.Event{Kind: alerts.KindUpgradeFailed, Severity: "critical",
    Title: "Upgrade to v1.4.0 failed", Detail: "project abc... did not come back; rolled back", Ref: ""})
```

`Notify` uses the node's default `Notifier` (`alerts.Configure(cfg, log)` sets it; the daemon does at start). Without one it does nothing and returns nil, so raising an event never fails the work that raised it. Every event is also logged under the message key `alert`, with `kind`, `severity`, `ref`, `title` and `detail`, whether or not a destination exists.

## Kinds

`Kind` and `Severity` are plain strings. The daemon's checker raises:

| Kind | When | Severity |
|---|---|---|
| `disk_low` | the state volume is under `[health] disk_low_percent` or `disk_low_gb` free | warning; critical under half |
| `backup_failed` | a project's (or the system cluster's) newest backup failed, or the newest completed one is stale | warning |
| `project_unhealthy` | a project's Postgres, Auth or REST does not answer, or a shared service lacks its tenant | critical |
| `certificate_expiring` | a certificate is under `[health] certificate_warn_days` from its end | warning; critical when expired |
| `node_unhealthy` | the registry, the system cluster or a shared service is not healthy | warning; critical for the registry and system Postgres |
| `update_available` | a release newer than the running version exists | info, once per version |

Other parts of the node raise `upgrade_started`, `upgrade_succeeded` and `upgrade_failed` and may raise any kind. Those three, `update_available` and `test` are announcements: each is sent when it happens, subject only to the hourly cap (and exempt from it, below). Everything else is a condition: sent once while it lasts.

Three places raise the `upgrade_*` events, each about its own scope:

| Raised by | About | Process |
|---|---|---|
| the daemon, from `lifecycle.Options.UpgradeNotify` (`internal/app`) | one project's upgrade: Studio's "Upgrade project", `POST /v1/projects/{ref}/upgrade`, and the upgrades the daemon closes after a crash. `Ref` is the project | the daemon |
| `supavise upgrade` (`internal/nodeupgrade` events, `cmd/supavise/upgrade_alerts.go`) | the node's upgrade: started, succeeded, failed and rolled back (warning), failed and needs the operator (critical), with the versions and the halted project | the CLI, as root, also when `supavise update run` starts it |
| `supavise rollback` | the node's rollback: started, succeeded, failed (critical) | the CLI, as root |

The rollout of a node upgrade runs project upgrades in worker processes that have no hook, so a node upgrade is one set of messages and not one per project. Neither the maintenance window nor the upgrade marker holds an `upgrade_*` event back: they quiet the checker's `project_unhealthy` and `node_unhealthy`, and the operator who starts an upgrade inside a window they announced still hears that it failed. A run that is refused before it changes anything raises nothing.

## De-duplication, recovery and the cap

- A condition has a key (`Event.Key`, by default `Kind/Ref`). The same key is not sent again until `[alerts] repeat_hours` (12) have passed since it was last sent; then it is a reminder.
- `Event{Resolved: true, Key: ...}` sends a recovery, once, and only for a problem that was sent. The checker sends one when a condition clears, including after a daemon restart (the state is on disk).
- At most `[alerts] max_per_hour` (20) notifications go out in any hour. A critical alert, the `upgrade_*` events and `update_available` (one message per version, recorded as told once sent) ignore the cap, so a burst of conditions cannot swallow the message that says an upgrade failed. Any other alert held back by the cap is counted once however often it is asked again, and the next notification that goes out says how many were held back.
- A notification that fails at every destination is not recorded as sent, so the next check sends it. One working destination is enough to count it as sent; the others' failures are logged.
- `min_severity` drops what is below it (`info`, `warning`, `critical`; default `info`).

The state is `<state_dir>/system/alerts.json`, guarded by a file lock so the daemon and a CLI run never both decide to send the same alert. The lock is held while the decision is recorded and again if a delivery has to be undone, not while the message travels (a webhook retry and SMTP can take 40 seconds): the decision reserves the notification, so a second process that asks meanwhile sees a duplicate, and a delivery that fails everywhere gives the reservation back.

**Root and the supavise user.** `sudo supavise upgrade` raises events as root, and the daemon runs as the `supavise` user. When the effective user is root, the state file and the lock file go to the owner of `<state_dir>/system` (the same rule as `internal/artifacts`), so a root run never leaves files the daemon cannot open. A command that calls `alerts.Configure` and `Notify` needs nothing else; the lead's merge wiring should not hand events to the daemon over another channel. It is a file and not the registry because the alerts that matter most are about the system cluster that holds the registry.

## The checker

`Checker.Run` runs in the daemon. Thirty seconds after start, then every `[alerts] check_interval_seconds` (60), it takes a fresh `health.Report` and derives the conditions above. A condition has to last `[alerts] unhealthy_after_seconds` (180) before it is sent, so a project that restarts, or a node that is mid-upgrade, pages nobody. More than five unhealthy projects at once are one alert ("Many projects are not healthy") that lists the first ten. The group is raised once more than five projects have been down for the debounce (it is as old as its sixth-oldest member), so a failure that grows from three projects to eight sends the group alert and no recovery messages for the three. Each project stays a condition of its own while grouped: one that recovers is told as recovered, and when the group shrinks to five or fewer it stands down without a recovery message (`Notifier.Forget`, because projects are still down) and the remaining projects are reported one by one. A shared service that cannot be reached is reported once, as itself, not once per project.

While an upgrade runs (`<state_dir>/system/upgrade.json`, see `internal/notice`) or an announced maintenance window is open, the checker raises and resolves no `project_unhealthy` and no `node_unhealthy` below critical: the operator caused that downtime, and the upgrade reports its own events. A critical `node_unhealthy` (the system cluster or the registry down, or any shared service that fails outright) is not suppressed; it still has to last the debounce, so a restart does not page. It also still raises and resolves `disk_low` (an upgrade takes fresh backups, so the disk is most likely to fill then), `backup_failed` and `certificate_expiring`. The debounce of what was held back starts over when the window ends.

Suppression fails closed. An upgrade marker counts for two hours after its last sign of life (`started_at` or the file's modification time, so the upgrade should rewrite it at each phase change), and one with neither does not count; a crashed upgrade therefore stops silencing alerts soon. A window longer than 24 hours is refused unless the operator passes `--allow-long`, and one that was not extended stops quieting alerts 24 hours after it starts, even if its file says otherwise.

**Updates.** Once a day (`[update] check_interval`, read by `internal/health`) the checker calls `health.CheckUpdate`, which records the newest release in `update.json`. A release the operator was not told about raises `update_available` once, with the next step (`supavise upgrade --check`); the record keeps the version so it is not sent twice. With no destination configured nothing is sent and the version stays unannounced until one is. Nothing about an available update reaches the dashboard.

## Destinations

`[alerts]` in `config.toml`:

```toml
[alerts]
email_to = "ops@example.com, oncall@example.com"   # through [mail]: smtp_host and smtp_from are required
check_interval_seconds = 60
unhealthy_after_seconds = 180
repeat_hours = 12
max_per_hour = 20
min_severity = "info"

[[alerts.webhooks]]
url = "https://hooks.example.com/supavise"
secret = "a long random string"                    # optional: signs the body
```

`config.Validate` refuses a webhook that is not an http or https URL, an unknown severity, a negative number, and `email_to` without `[mail]`. The webhook list has no environment override; secrets sit in `config.toml` in plain text like the backup keys, so keep the file mode 0600. A changed `[alerts]` takes effect when the daemon restarts.

**Webhook.** A JSON `POST`:

```json
{"version":1,"kind":"disk_low","severity":"warning","title":"Disk space is low",
 "detail":"8% free (4.1 GiB of 50.0 GiB) on /var/lib/supavise","ref":"","resolved":false,
 "node":"example.com","time":"2026-10-07T12:00:00Z",
 "text":"[warning] example.com: Disk space is low\n8% free ..."}
```

`text` is a ready sentence, so Slack, Mattermost and Rocket.Chat incoming webhooks show it as the message. The headers are `Content-Type: application/json`, `X-Supavise-Event: <kind>` and, with a `secret`, `X-Supavise-Timestamp: <unix seconds>` and `X-Supavise-Signature: sha256=<hex>`, the HMAC-SHA256 under the secret of the timestamp, a dot and the raw body (the scheme Stripe uses). A receiver checks both:

```python
ts = request.headers["X-Supavise-Timestamp"]
expected = "sha256=" + hmac.new(secret, ts.encode() + b"." + raw_body, hashlib.sha256).hexdigest()
ok = hmac.compare_digest(expected, request.headers["X-Supavise-Signature"]) and abs(time.time() - int(ts)) < 300
```

The timestamp is signed, so a receiver can refuse a captured request that is replayed outside a tolerance window (five minutes above); a retry after a 5xx carries a fresh timestamp. A 5xx or 429 answer is retried once after two seconds; any other non-2xx answer is a failure. Logs, errors and the results of `supavise alerts test` name a webhook by scheme and host only, because chat services keep their secret in the path or query; the HTTP client's own error text, which carries the whole URL, is rewritten before it is stored.

**Email.** A plain-text message through the `[mail]` relay (STARTTLS when the server offers it, implicit TLS on port 465, PLAIN authentication when `smtp_user` is set). The subject is `[Supavise] <severity>: <node>: <title>`; line breaks in any header value are removed.

## CLI

```
supavise alerts test    # a test alert to every destination; prints each result; exit status 1 if none is configured or any failed
supavise alerts list    # the alerts that were sent and are not resolved
```

## Tests

```
go test ./internal/alerts/
```

`httptest` endpoints for the webhook body, headers, signature (checked against an independent HMAC) and retry; a fake SMTP server for the mail path; a clock the tests move for the repeat interval, the hourly cap (and what it never holds back) and the cap's count of held alerts; a webhook that is unreachable, with a secret path that must appear in no log line or error; a second notifier that must not wait for a slow delivery; root handing the state files to the state owner; state shared between two notifiers; recovery after a restart; the checker's debounce, flap handling, grouping, the unreachable-service rule, the quiet period during an upgrade or a maintenance window (a crashed upgrade's marker, a marker without `started_at`, a month-long window, a critical node problem), a group of unhealthy projects that grows and shrinks without false recoveries, and `update_available` once per version against a fake GitHub API.
