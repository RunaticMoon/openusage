package detect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectDSHPersistenceRootWithoutCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("DSH_HOME", filepath.Join(root, "custom"))
	t.Setenv("PATH", "")
	t.Setenv("OPENUSAGE_DETECT_BIN_DIRS", "")
	var absent Result
	detectDSH(&absent)
	if len(absent.Accounts) != 0 {
		t.Fatal("absent dsh detected")
	}
	sessions := filepath.Join(root, "custom", "sessions")
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	var found Result
	detectDSH(&found)
	if len(found.Accounts) != 1 || found.Accounts[0].Provider != "dsh" || found.Accounts[0].Path("sessions_dir", "") != sessions {
		t.Fatalf("detection failed: count=%d", len(found.Accounts))
	}
}
