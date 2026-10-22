package detect

import (
	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/jev"
)

func detectJev(result *Result) {
	root := jev.DefaultUsageDir()
	if root == "" || !dirExists(root) {
		return
	}
	result.Tools = append(result.Tools, DetectedTool{Name: "Jev usage metadata", ConfigDir: root, Type: "local"})
	account := core.AccountConfig{ID: jev.ID, Provider: jev.ID, Auth: "local"}
	account.SetPath("usage_dir", root)
	addAccount(result, account)
}
