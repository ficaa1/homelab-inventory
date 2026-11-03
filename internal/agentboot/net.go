package agentboot

import "net"

func loopback(s string) bool { ip := net.ParseIP(s); return ip != nil && ip.IsLoopback() }
