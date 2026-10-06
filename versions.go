// Package sbctl embeds repository-level data files into the binary.
package sbctl

import _ "embed"

// VersionsYAML is the versions.yaml this binary was built and tested with.
//
//go:embed versions.yaml
var VersionsYAML []byte
