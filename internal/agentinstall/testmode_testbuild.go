//go:build agentinstalltest

package agentinstall

import "os"

// Only the disposable integration binary can exercise fake systemd with prefix.
func manageSystemd(o Options) bool {
	return o.Prefix == "" || os.Getenv("AGENTINSTALL_TEST_SYSTEMD") == "1"
}
