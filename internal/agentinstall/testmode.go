//go:build !agentinstalltest

package agentinstall

func manageSystemd(o Options) bool { return o.Prefix == "" }
