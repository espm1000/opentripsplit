package opentripsplit

import (
	"regexp"
	"testing"
)

func TestVersion(t *testing.T) {
	if v := Version(); !regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(v) {
		t.Errorf("Version() = %q, want the manifest's semver", v)
	}

	old := commit
	t.Cleanup(func() { commit = old })
	commit = "abc1234"
	if v := Version(); v != "abc1234" {
		t.Errorf("branch build: Version() = %q, want the commit", v)
	}
}

func TestReleaseVersion(t *testing.T) {
	for in, want := range map[string]string{
		`{".":"1.2.3"}`: "1.2.3",
		`{"x":"1.2.3"}`: "unknown",
		`not json`:      "unknown",
		`{".":""}`:      "unknown",
	} {
		if got := releaseVersion([]byte(in)); got != want {
			t.Errorf("releaseVersion(%s) = %q, want %q", in, got, want)
		}
	}
}
