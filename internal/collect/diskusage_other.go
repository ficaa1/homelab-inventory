//go:build !unix

package collect

// diskUsage is unavailable off Unix. The binary targets Linux; this stub only
// exists so `go build ./...` and editor tooling work on other platforms.
func diskUsage(string) (total, free, avail uint64, ok bool) {
	return 0, 0, 0, false
}
