# internal/notice

What the operator tells the dashboard's users: a maintenance window announced with `supavise maintenance announce`, and the marker an upgrade writes while it runs. `supavise status` and `/healthz/detail` show them to the operator; the Studio banner does not (see The banner).

Both live in small JSON files under `<state_dir>/system/` (0644, written atomically), so a reader needs neither the registry nor the daemon's memory, and a file the CLI writes is seen by the next read.

| File | Writer | Content |
|---|---|---|
| `maintenance.json` | `supavise maintenance announce` / `clear` | `id`, `message`, `starts_at`, `ends_at`, `lead_seconds`, `announced_at` |
| `upgrade.json` | the upgrade (`supavise upgrade`) | `phase`, `from`, `to`, `started_at` |

An upgrade counts as running while `phase` is not a finished one (`done`, `complete`, `succeeded`, `failed`, `rolled_back`, `aborted`, `refused`, empty) and its last sign of life is under two hours old, so a marker left by a process that died stops counting soon. The last sign of life is `started_at` or the file's modification time, whichever is later (`Upgrade.Heartbeat`); a marker with neither does not count. The upgrade should rewrite the file at each phase change, which keeps it alive, and remove it or set a finished phase when it ends. An unreadable file reads as "no upgrade".

A maintenance window longer than 24 hours (`MaxWindow`) is refused unless the operator passes `--allow-long` (`extended` in the file). `Quiet(now)`, which the alert checker uses, is true only while the window is open and, unless extended, less than 24 hours after its start, so a file edited by hand cannot silence alerts for a month.

## The banner

`BannerJSON()` is the answer of `/api/incident-banner`, and it is `{"incidents":[]}` always. This Studio build has no feature flags (ConfigCat is not configured), so it draws the legacy banner for any incident: a fixed title ("We are investigating a technical issue", or "Project creation may be impacted in some regions" to a user with no project) and a link to Supabase's status page. It takes no text from the answer. Planned maintenance and an upgrade the operator started would read as an unexplained outage, so they are not sent.

Hosted shows maintenance through the incident.io status page (`StatusBanner`, kinds incident, maintenance and upcoming maintenance, with title and times) behind the `incidentIoStatusPage` flag. Showing the operator's own words here needs either a Studio patch that reads `title` and `message` from the incident, or serving that status page route with the flag on. Until one of them exists, the notices reach the operator through `supavise status`, `/healthz/detail` and the alerts.

`Banner(paths, now)` and `IncidentsJSON` compute what to serve then: the announced maintenance while its banner is active (from `lead_seconds` before it opens when the operator gave `--notice`), and an upgrade that is running. Each incident has the three fields Studio reads at the pinned tag (`apps/studio/data/platform/incident-banner-query.ts`): `id` (what Studio stores when a user dismisses the banner, so every announcement and every upgrade has its own), `show_banner: "force"` and `metadata` with `force: true`, `affected_regions: null` and `affects_project_creation: false`, plus `kind`, `title`, `message`, `starts_at` and `ends_at`. An update that is merely available is never listed: the dashboard's users cannot act on it.

## Tests

`go test ./internal/notice/ ./internal/proxy/`: validation of an announcement, the window rules, the file round trip, which upgrade phases count as running, tolerant reads, the shape of the incidents against what Studio reads, an update never appearing, and the proxy's answer staying empty.
