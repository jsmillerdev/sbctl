# internal/notice

What the operator tells the dashboard's users: a maintenance window announced with `supavise maintenance announce`, and the marker an upgrade writes while it runs. The proxy turns the active ones into the answer of Studio's `/api/incident-banner`; `supavise status` shows them to the operator.

Both live in small JSON files under `<state_dir>/system/` (0644, written atomically), so the answer needs neither the registry nor the daemon's memory, and a file the CLI writes is seen by the next request.

| File | Writer | Content |
|---|---|---|
| `maintenance.json` | `supavise maintenance announce` / `clear` | `id`, `message`, `starts_at`, `ends_at`, `lead_seconds`, `announced_at` |
| `upgrade.json` | the upgrade (`supavise upgrade`) | `phase`, `from`, `to`, `started_at` |

An upgrade counts as running while `phase` is not a finished one (`done`, `complete`, `succeeded`, `failed`, `rolled_back`, `aborted`, `refused`, empty) and it began under 12 hours ago, so a marker left by a process that died does not show a banner for days. The upgrade should remove the file or set a finished phase when it ends. An unreadable file reads as "no upgrade".

## The banner

`Banner(paths, now)` returns the notices to show; `BannerJSON` renders `{"incidents":[...]}`. Each incident has the three fields Studio reads at the pinned tag (`apps/studio/data/platform/incident-banner-query.ts`): `id` (what Studio stores when a user dismisses the banner, so every announcement and every upgrade has its own), `show_banner: "force"` and `metadata` with `force: true`, `affected_regions: null` and `affects_project_creation: false`. `force` shows the banner to everyone, including users with no project, whatever their regions. The incident also carries `kind` (`maintenance` or `upgrade`), `title`, `message`, `starts_at` and `ends_at` for anything else that reads the answer.

A maintenance window shows while it is open, and from `lead_seconds` before it opens when the operator gave `--notice`. An update that is merely available is never shown here: the dashboard's users cannot act on it, and the operator hears about it through `supavise status` and the alerts.

**Studio draws its own text.** This Studio build has no feature flags (ConfigCat is not configured), so it uses the older banner, which takes only the incident list from the answer and shows a fixed title ("We are investigating a technical issue", or "Project creation may be impacted in some regions" to a user with no project) with a link to Supabase's status page. The announcement's message is in the answer and in `supavise status`, but Studio does not display it. Showing the operator's own words needs a change in Studio: a fourth patch that reads `title` and `message` from the incident, or serving the incident.io status page route together with the `incidentIoStatusPage` flag. That is why `--notice` defaults to 0: a banner that says "technical issue" a day before planned work would mislead.

## Tests

`go test ./internal/notice/ ./internal/proxy/`: validation of an announcement, the window rules, the file round trip, which upgrade phases count as running, tolerant reads, the answer's shape against what Studio reads, an update never appearing, and the proxy answering with the files of its state directory without a restart.
