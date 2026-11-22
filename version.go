// Package opentripsplit holds what has to live at the module root: the
// release-please manifest is only embeddable from here.
package opentripsplit

import (
	_ "embed"
	"encoding/json"
)

//go:embed .release-please-manifest.json
var manifest []byte

// commit is the short commit SHA of a branch build, set at build time:
//
//	go build -ldflags "-X github.com/espm1000/opentripsplit.commit=abc1234"
//
// Release builds leave it empty.
var commit string

// Version is what the running build calls itself: the commit SHA for a
// branch build, otherwise the release version from the manifest, which
// release-please bumps in each release commit.
func Version() string {
	if commit != "" {
		return commit
	}
	return releaseVersion(manifest)
}

// releaseVersion reads the root package's version from a release-please
// manifest, or "unknown" if it can't.
func releaseVersion(b []byte) string {
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil || m["."] == "" {
		return "unknown"
	}
	return m["."]
}
