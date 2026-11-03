package agentboot

import (
	"context"
	"errors"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

// LockDirWait retries contention only, bounded by both wait and ctx. A zero
// wait preserves the non-blocking behavior used by ordinary scheduled runs.
func LockDirWait(ctx context.Context, dir string, wait time.Duration, onWait func()) (func(), error) {
	if wait <= 0 {
		return fsx.LockDir(dir)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	announced := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := fsx.LockDir(dir)
		if !errors.Is(err, fsx.ErrLocked) {
			return unlock, err
		}
		if !announced && onWait != nil {
			onWait()
			announced = true
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
