//go:build unix

package collect

import "syscall"

// diskUsage returns total, free (to root) and available (to unprivileged
// users) bytes for the filesystem containing path.
//
// Free and available differ by the reserved-blocks percentage, which on a
// default ext4 is 5%. Reporting available is what `df` shows and what you
// actually have; reporting total-free as used is what `df` calls Use%. Both
// are kept so the numbers reconcile with what you see on the terminal.
func diskUsage(path string) (total, free, avail uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, false
	}
	bsize := uint64(st.Bsize)
	if bsize == 0 {
		return 0, 0, 0, false
	}
	return uint64(st.Blocks) * bsize, uint64(st.Bfree) * bsize, uint64(st.Bavail) * bsize, true
}
