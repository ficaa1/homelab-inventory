package agentproc

import "syscall"

func prepare() error                   { return nil }
func attributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
func reap(int)                         {}
