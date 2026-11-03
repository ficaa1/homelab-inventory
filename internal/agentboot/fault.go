//go:build !agentbootfault

package agentboot

func faultPoint(string) error { return nil }
