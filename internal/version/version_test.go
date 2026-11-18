package version

import (
	"os"
	"regexp"
	"testing"
)

// TestMatchesVivaToml keeps Version and viva.toml's [app] version equal, so
// the packaged bundle and the update check agree on what is installed.
func TestMatchesVivaToml(t *testing.T) {
	data, err := os.ReadFile("../../viva.toml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`).FindSubmatch(data)
	if m == nil {
		t.Fatal("viva.toml has no version")
	}
	if string(m[1]) != Version {
		t.Fatalf("viva.toml version %s != version.Version %s", m[1], Version)
	}
}
