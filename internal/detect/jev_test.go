package detect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectJevHostMetadataWithoutCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PASEO_HOME", filepath.Join(root, "custom-paseo"))
	var absent Result
	detectJev(&absent)
	if len(absent.Accounts) != 0 {
		t.Fatal("missing metadata directory detected")
	}
	dir := filepath.Join(root, "custom-paseo", "usage-limits", "jev")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var found Result
	detectJev(&found)
	if len(found.Accounts) != 1 || found.Accounts[0].Provider != "jev" || found.Accounts[0].Auth != "local" || found.Accounts[0].Path("usage_dir", "") != dir {
		t.Fatalf("unexpected Jev detection: %+v", found.Accounts)
	}
}
