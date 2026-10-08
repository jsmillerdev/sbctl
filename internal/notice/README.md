# internal/notice

What the operator tells the dashboard's users: a maintenance window announced with `supavise maintenance announce`, and the marker an upgrade writes while it runs. `supavise status` and `/healthz/detail` show them to the operator; the Studio banner does not (see The banner).

Both live in small JSON files under `<state_dir>/system/` (0644, written atomically), so a reader needs neither the registry nor the daemon's memory, and a file the CLI writes is seen by the next read.

| File | Writer | Content |
|---|---|---|
| `maintenance.json` | `supavise maintenance announce` / `clear` | `id`, `message`, `starts_at`, `ends_at`, `lead_seconds`, `announced_at` |
| `upgrade.json` | the upgrade (`supavise upgrade`, `supavise rollback`) | `phase`, `from`, `to`, `started_at`, `pid`, `detail` |

`supavise upgrade` writes the file at every phase change and every ten minutes while it runs. Its phases are `preparing`, `switching`, `services`, `projects`, `verifying` and `rolling_back`; it ends with `done`, `rolled_back`, `failed` or `refused`, and the file stays. `pid` is the process that runs it: another upgrade refuses to start while a fresh marker's process is alive. The daemon reads the marker too: while an upgrade runs, its start of the shared services stops at the first failure (`fleet.Deps.HaltOnFailure`).

An upgrade counts as running while `phase` is not a finished one (`done`, `complete`, `succeeded`, `failed`, `rolled_back`, `aborted`, `refused`, empty) and its last sign of life is under two hours old, so a marker left by a process that died stops counting soon. The last sign of life is `started_at` or the file's modification time, whichever is later (`Upgrade.Heartbeat`); a marker with neither does not count. The upgrade should rewrite the file at each phase change, which keeps it alive, and remove it or set a finished phase when it ends. An unreadable file reads as "no upgrade".

A maintenance window longer than 24 hours (`MaxWindow`) is refused unless the operator passes `--allow-long` (`extended` in the file). `Quiet(now)`, which the alert checker uses, is true only while the window is open and, unless extended, less than 24 hours after its start, so a file edited by hand cannot silence alerts for a month.

## The banner

`BannerJSON()` is the answer of `/api/incident-banner`, and it is `{"incidents":[]}` always. This Studio build has no feature flags (ConfigCat is not configured), so it draws the legacy banner for any incident: a fixed title ("We are investigating a technical issue", or "Project creation may be impacted in some regions" to a user with no project) and a link to Supabase's status page. It takes no text from the answer, so planned maintenance and an upgrade the operator started would read as an unexplained outage, and they are not sent.

Showing the operator's own words needs either a Studio patch that reads `title` and `message` from the incident, or serving the incident.io status page route (`StatusBanner`, behind the `incidentIoStatusPage` flag) with the flag on. Until then, the notices reach the operator through `supavise status`, `/healthz/detail` and the alerts.

`Banner(paths, now)` and `IncidentsJSON` compute what to serve once either exists: the announced maintenance while its banner is active (from `lead_seconds` before it opens when the operator gave `--notice`), and an upgrade that is running. Each incident has the fields Studio reads at the pinned tag (`apps/studio/data/platform/incident-banner-query.ts`): `id` (what Studio stores when a user dismisses the banner, so every announcement and every upgrade has its own), `show_banner: "force"`, `metadata` with `force: true`, `affected_regions: null` and `affects_project_creation: false`, plus `kind`, `title`, `message`, `starts_at` and `ends_at`. An update that is merely available is never listed: the dashboard's users cannot act on it.

## Tests

`go test ./internal/notice/ ./internal/proxy/` (the `test` job of `ci.yml`) covers the announcement and window rules, which upgrade phases count as running, the shape of the incidents against what Studio reads, and the proxy's empty answer. The `install-e2e` job of `linux.yml` announces and clears a window and checks that `/api/incident-banner` stays empty.
