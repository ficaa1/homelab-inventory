package agentproc

import (
	"sync"
	"syscall"
)

var once sync.Once
var setupErr error

func prepare() error {
	once.Do(func() {
		_, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0)
		if e != 0 {
			setupErr = e
		}
	})
	return setupErr
}
func attributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
func reap(group int) {
	for {
		_, e := syscall.Wait4(-group, nil, 0, nil)
		if e == syscall.EINTR {
			continue
		}
		if e != nil {
			return
		}
	}
}
