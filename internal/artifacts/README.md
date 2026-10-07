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
- **Unpacking:** zstd + tar, no path traversal, no links that leave the destination, no
  device or FIFO entries, setuid/setgid/sticky dropped, modes and symlinks kept. The tree is
  unpacked next to its destination and renamed into place, so a directory either does not
  exist or is complete. The artifact root is mode 0755 so that the `supavise` user can run an
  artifact that root fetched, and when root fetches, the unpacked tree is handed to the owner of
  `state_dir` (the `supavise` user): the Postgres launcher chmods a script inside its artifact on
  first boot, which only the file's owner may do. A `.supavise-artifact.json` marker records tag, platform, digest
  and source.
- **CLI:** `supavise artifacts fetch [service...] [--studio]`, `supavise artifacts list`.

## Test

```
go test ./internal/artifacts/
SUPAVISE_TEST_ARCHIVE=/path/to/real.tar.zst go test -run RealArchive ./internal/artifacts/
```

Tests use small synthetic archives and an `httptest` release server; no real downloads.

## Not done

- No mirror fall-through (GHCR, S3); one `base_url`.
- No garbage collection of old artifact versions or of the archive cache.
- Archive signatures are not checked; integrity comes from `SHA256SUMS` over HTTPS.
