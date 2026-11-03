// Package agentproc supervises only processes started by the caller. Discover's
// external-command policy remains in sysexec; this package is for the updater.
package agentproc

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"
)

func Run(ctx context.Context, path string, args []string, in io.Reader, out io.Writer) error {
	if err := prepare(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = attributes()
	cmd.Cancel = func() error {
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(e, syscall.ESRCH) {
			return nil
		}
		return e
	}
	cmd.WaitDelay = 100 * time.Millisecond
	if e := cmd.Start(); e != nil {
		return e
	}
	e := cmd.Wait()
	// A successful leader may leave a hung descendant holding the output pipe.
	// Always terminate the whole owned group, and reap adopted children on Linux.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	reap(cmd.Process.Pid)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return e
}
