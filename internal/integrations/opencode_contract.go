package integrations

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var openCodeVersionPattern = regexp.MustCompile(`(?m)^(?:opencode[ /])?v?([0-9]+)\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?\s*$`)

// An installed plugin must be rendered for its host: the older v1 loader calls
// every export, whereas v2 requires an object. A major upgrade needs reinstall.
func renderOpenCodeContract(content, version string) (string, string) {
	contract := "legacy-v1"
	export := "export default OpenUsageTelemetry"
	warning := ""
	match := openCodeVersionPattern.FindStringSubmatch(strings.TrimSpace(version))
	if len(match) == 2 && match[1] == "2" {
		contract = "v2"
		export = `export default { id: "openusage-telemetry", setup }`
	} else if len(match) != 2 || match[1] != "1" {
		warning = "OpenCode version could not be confirmed as supported v1/v2; installed the legacy v1 plugin. Verify opencode on PATH and reinstall before using v2."
	}
	marker := "// openusage-opencode-contract: " + contract + "\n"
	return strings.ReplaceAll(content, "__OPENUSAGE_OPENCODE_EXPORT__", marker+export), warning
}

func installedOpenCodeVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "opencode", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
