# internal/artifacts

Fetches the slim-services `tar.zst` releases pinned in `internal/versions/versions.yaml` and unpacks them
under `<state_dir>/artifacts/<service>/<tag>/` (`config.Paths.Artifact`).

- **Versions:** the `internal/versions/versions.yaml` embedded in the binary, or `[artifacts] versions_file`.
- **Platform:** `platform` in the config, or the running `<goos>-<goarch>`; builds exist for
  `linux-amd64`, `linux-arm64` and `darwin-arm64`.
- **Verification:** the archive must match its line in the release `SHA256SUMS`
  (`<base_url>/<tag>/SHA256SUMS`). A cached archive with another digest is downloaded again.
  Our Studio build (`supavise artifacts fetch --studio`) is verified against
  `[studio] artifact_sha256`.
- **Studio build:** the build is named by the upstream tag and the patch set,
  `<studio.tag>-p<studio.patchset>` (`Versions.StudioBuild`), and lives under
  `<state_dir>/artifacts/studio/<build>/`. `Tag(SvcStudio)`, `Pins()` and so the GC, the upgrade plan
  and the release manifest all use that name, so a release that changes only the patch set moves
  Studio. A versions file without `patchset` (every release up to v0.2.0) names the build by the tag
  alone. A directory holds the build its name says and no other: a `[studio] artifact_url` whose file
  name is the release asset of another build (`supavise-studio-<build>-<platform>.tar.zst`) is
  refused before the download, and an archive whose `share/supavise/build-info.json` names another
  build is refused before it is put in place. A node that a release before the patch set installed
  has the build under `artifacts/studio/<tag>/`; when that build is the pinned one (its build-info
  says so, or, for a stand-in without build-info, its marker has the configured digest), the fetch
  links it into the new directory file by file (hard links, `adopted_from` in the marker) instead of
  downloading it, and the old directory stays for the Studio that runs from it and for a rollback.
- **Unpacking:** zstd + tar, no path traversal, no links that leave the destination, no
  device or FIFO entries, setuid/setgid/sticky dropped, modes and symlinks kept. The tree is
  unpacked next to its destination and renamed into place, so a directory either does not
  exist or is complete. The artifact root is mode 0755 so that the `supavise` user can run an
  artifact that root fetched, and when root fetches, the unpacked tree is handed to the owner of
  `state_dir` (the `supavise` user): the Postgres launcher chmods a script inside its artifact on
  first boot, which only the file's owner may do. A `.supavise-artifact.json` marker records tag, platform, digest
  and source.
- **By tag:** `Store.DirFor(svc, tag)` and `Store.FetchTag(ctx, svc, tag)` find and fetch a release that is
  not the pinned one. A project runs the versions it was last upgraded to, so the lifecycle renders
  its units from `DirFor` of the project's recorded tags (`internal/lifecycle/README.md`, "Service
  versions and project upgrades").
- **Release history and garbage collection:** the daemon appends the pins of the running release to
  `<state_dir>/artifacts/.releases.json` when they change (`Store.RecordPins`, at start, the last 20
  kept). `Store.KeepSet` and `Store.GC` remove the unpacked artifacts that nothing needs: not the
  pins, not the newest recorded release (what the daemon last started with; the binary that runs GC
  may be newer than the daemon), not the last `[upgrade] keep_releases` releases of the history
  (default 3: the current one and the two before, so a rollback finds its artifacts), not any tag the caller names (the
  lifecycle names every version a project runs or is being upgraded to). Directories that start
  with a dot (the archive cache, unpacking in progress) are never touched. Entry points:
  `supavise artifacts gc [--dry-run] [--keep N]` and, after a successful `supavise projects upgrade`,
  `Engine.CollectArtifacts`.
- **CLI:** `supavise artifacts fetch [service...] [--studio]` (`--studio` without a service fetches Studio alone),
  `supavise artifacts list`, `supavise artifacts gc`.

## Tests

`go test ./internal/artifacts/` uses small synthetic archives and an `httptest` release server; it makes no
real downloads, and runs in the `test` job of the `ci` workflow. `SUPAVISE_TEST_ARCHIVE=/path/to/real.tar.zst
go test -run RealArchive ./internal/artifacts/` unpacks a real archive.

## Limits

- No mirror fall-through (GHCR, S3); one `base_url`.
- The archive cache (`.cache`) is never collected.
- Archive signatures are not checked; integrity comes from `SHA256SUMS` over HTTPS.
