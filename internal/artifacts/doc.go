// Package artifacts fetches the slim-services tar.zst releases pinned in versions.yaml,
// verifies their SHA-256 against the release SHA256SUMS, and unpacks them under
// <state_dir>/artifacts/<service>/<version>/.
package artifacts
