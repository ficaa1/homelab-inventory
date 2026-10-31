package main

import (
	"os"
	"strconv"

	"github.com/ficaa1/homelab-inventory/internal/backup"
)

// Administrators can raise limits for large history stores. Invalid values keep
// conservative defaults; these deployment bounds cannot be changed by a backup.
func configuredBackupLimits() backup.Limits {
	lim := backup.DefaultLimits()
	positive := func(key string, fallback, max int64) int64 {
		n, e := strconv.ParseInt(os.Getenv(key), 10, 64)
		if e != nil || n <= 0 || n > max {
			return fallback
		}
		return n
	}
	lim.Files = int(positive("BACKUP_MAX_FILES", int64(lim.Files), 10000000))
	lim.TotalBytes = positive("BACKUP_MAX_TOTAL_BYTES", lim.TotalBytes, 1<<50)
	lim.FileBytes = positive("BACKUP_MAX_FILE_BYTES", lim.FileBytes, lim.TotalBytes)
	if lim.FileBytes > lim.TotalBytes {
		lim.FileBytes = lim.TotalBytes
	}
	return lim
}
