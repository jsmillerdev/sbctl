# internal/selfupdate

`sbctl self-update` as a library: `Update(ctx, Options)` replaces a binary with a GitHub release after verifying it, `Latest` reads the release, `Newer` compares versions.

A release holds `sbctl-linux-amd64`, `sbctl-linux-arm64`, `SHA256SUMS` (sha256sum format) and `SHA256SUMS.sig`, the raw 64-byte ed25519 signature of the `SHA256SUMS` file. `deploy/release-assets.sh` produces them with `openssl pkeyutl -sign -rawin`; the OpenSSL signature in `testdata/` proves that Go's `crypto/ed25519` verifies what OpenSSL signs.

Order of events in `Update`: resolve the release, stop if it is not newer (or `Force`), download `SHA256SUMS` and the signature, verify the signature against the key (`release_key.pem` embedded in the binary, or `Options.Key`), read the checksum of the binary asset from the signed list, stream the binary into a temp file next to the target while hashing, compare, `chmod 0755`, run it with `--version`, hard-link the old file as `<name>.prev`, rename the new file over the target. Every failure before the rename leaves the installed binary and the directory as they were. No key (the placeholder file), an unsigned release, a list that omits the platform, a binary that does not match and a binary that does not start are all refusals with their own message (tests in `selfupdate_test.go`).

`release_key.pem` holds the public key. It is a placeholder until the maintainers commit one (`deploy/README.md`, Release signing); with the placeholder `Update` returns `ErrNoKey` before it touches the network. `DefaultRepo` and the key are the only build-time inputs.

Not done: downgrade protection beyond "newer unless `--version` or `--force`"; a signed release manifest with a minimum supported version; delta updates.
