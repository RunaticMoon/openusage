package detect

import (
	"path/filepath"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/dsh"
)

// DSH's persistence root follows DSH_HOME, independently of its named profiles.
// Discovery reads neither profiles nor credentials and never executes the CLI.
func detectDSH(result *Result) {
	bin := findBinary("dsh")
	sessions := dsh.DefaultSessionsDir()
	if bin == "" && (sessions == "" || !dirExists(sessions)) {
		return
	}
	result.Tools = append(result.Tools, DetectedTool{Name: "DeepSeek Harness", BinaryPath: bin, ConfigDir: filepath.Dir(sessions), Type: "cli"})
	account := core.AccountConfig{ID: dsh.ID, Provider: dsh.ID, Auth: "local", Binary: bin}
	account.SetPath("sessions_dir", sessions)
	addAccount(result, account)
}
