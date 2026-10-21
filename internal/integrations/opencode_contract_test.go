package integrations

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRenderOpenCodeContract(t *testing.T) {
	for _, tc := range []struct {
		version, contract string
		warning           bool
	}{
		{"1.2.0", "legacy-v1", false}, {"opencode 1.19.3", "legacy-v1", false},
		{"v2.0.32", "v2", false}, {"2.1.1", "v2", false}, {"2.1.1-dev.1", "v2", false},
		{"", "legacy-v1", true}, {"unrecognized", "legacy-v1", true}, {"3.0.0", "legacy-v1", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			got, warning := renderOpenCodeContract(opencodeTemplate, tc.version)
			if !strings.Contains(got, "openusage-opencode-contract: "+tc.contract) {
				t.Fatalf("contract marker missing for %q", tc.version)
			}
			if (warning != "") != tc.warning {
				t.Fatalf("warning=%q", warning)
			}
			if strings.Contains(got, "__OPENUSAGE_OPENCODE_EXPORT__") {
				t.Fatal("unrendered export")
			}
			if strings.Count(got, "export default ") != 1 {
				t.Fatal("exactly one default export required")
			}
			if tc.contract == "legacy-v1" && !strings.Contains(got, "export default OpenUsageTelemetry") {
				t.Fatal("v1 function export missing")
			}
		})
	}
}

func TestInstallOpenCodeSelectsHostContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	for _, tc := range []struct{ version, contract string }{{"1.2.0", "legacy-v1"}, {"v2.0.32", "v2"}, {"2.1.1", "v2"}} {
		t.Run(tc.version, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte("#!/bin/sh\nprintf '%s\\n' '"+tc.version+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			result, err := Install(opencodeDef(), testDirs(root))
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(result.TemplateFile)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "openusage-opencode-contract: "+tc.contract) {
				t.Fatalf("installer selected wrong contract for %s", tc.version)
			}
		})
	}
}
