// Package versions embeds versions.yaml, the pinned upstream releases, into the binary.
package versions

import _ "embed"

// VersionsYAML is the versions.yaml this binary was built and tested with.
//
//go:embed versions.yaml
var VersionsYAML []byte
