//go:build agentbootfault

package agentboot

import (
	"os"
	"strconv"
	"syscall"
)

var faultHits int

func faultPoint(point string) error {
	if point != os.Getenv("AGENTBOOT_FAULT") {
		return nil
	}
	skip, _ := strconv.Atoi(os.Getenv("AGENTBOOT_FAULT_SKIP"))
	if faultHits < skip {
		faultHits++
		return nil
	}
	switch os.Getenv("AGENTBOOT_FAULT_MODE") {
	case "enospc":
		return syscall.ENOSPC
	case "eio":
		return syscall.EIO
	default:
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
}
