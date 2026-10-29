// Package overview aggregates the read-only opening dashboard view.
package overview

import "time"

type Unavailable struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Fleet       any       `json:"fleet"`
	Attention   any       `json:"attention"`
	Storage     any       `json:"storage"`
	ZFS         any       `json:"zfs"`
	Backups     any       `json:"backups"`
	UnitsFailed any       `json:"units_failed"`
}
type Fleet struct {
	HostsTotal     int      `json:"hosts_total"`
	HostsReporting int      `json:"hosts_reporting"`
	Stale          []Stale  `json:"stale"`
	AgentsOutdated []string `json:"agents_outdated"`
	Alerts         Counts   `json:"alerts"`
}
type Counts struct {
	Critical int    `json:"critical"`
	Warning  int    `json:"warning"`
	Info     int    `json:"info"`
	Silenced int    `json:"silenced"`
	Recent   Recent `json:"recent"`
}
type Stale struct {
	Host     string     `json:"host"`
	LastSeen *time.Time `json:"last_seen"`
	AgeS     *int64     `json:"age_s"`
	Severity string     `json:"severity"`
}
type Attention struct {
	Host     string     `json:"host"`
	Rule     string     `json:"rule"`
	Subject  string     `json:"subject"`
	Severity string     `json:"severity"`
	Since    *time.Time `json:"since"`
	Summary  string     `json:"summary"`
}
type Storage struct {
	Host          string    `json:"host"`
	Kind          string    `json:"kind"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	UsedBytes     *int64    `json:"used_bytes"`
	SizeBytes     *int64    `json:"size_bytes"`
	Pct           *float64  `json:"pct"`
	Delta24hBytes *int64    `json:"delta_24h_bytes"`
	Spark         []float64 `json:"spark"`
	Forecast      Forecast  `json:"forecast"`
}
type Forecast struct {
	Status string     `json:"status"`
	FullAt *time.Time `json:"full_at"`
	Reason string     `json:"reason"`
}
type Pool struct {
	Scan      string     `json:"scan"`
	Status    string     `json:"status"`
	Host      string     `json:"host"`
	Pool      string     `json:"pool"`
	Health    string     `json:"health"`
	Pct       float64    `json:"pct"`
	Scrub     Scrub      `json:"scrub"`
	Snapshots *Snapshots `json:"snapshots"`
}
type Scrub struct {
	Status     string     `json:"status"`
	State      *string    `json:"state"`
	FinishedAt *time.Time `json:"finished_at"`
	AgeDays    *int       `json:"age_days"`
	Errors     *int       `json:"errors"`
}
type Snapshots struct {
	Status        string `json:"status"`
	Count         int    `json:"count"`
	NewestAgeDays *int   `json:"newest_age_days"`
}
type Backups struct {
	Counts BackupCounts `json:"counts"`
	Jobs   []BackupJob  `json:"jobs"`
	Guests []Guest      `json:"guests"`
}
type BackupCounts struct {
	Covered   int `json:"covered"`
	Skipped   int `json:"skipped"`
	Uncovered int `json:"uncovered"`
	Unknown   int `json:"unknown"`
}
type BackupJob struct {
	Storage   string     `json:"storage"`
	Host      string     `json:"host"`
	ID        string     `json:"id"`
	Schedule  string     `json:"schedule"`
	NextRun   *time.Time `json:"next_run"`
	Retention string     `json:"retention"`
	Enabled   bool       `json:"enabled"`
}
type Guest struct {
	ExcludedDisks          []string `json:"excluded_disks"`
	Host                   string   `json:"host"`
	Coverage               string   `json:"coverage"`
	Jobs                   []string `json:"jobs"`
	SilenceReason          string   `json:"silence_reason,omitempty"`
	NewestArchiveSizeBytes *uint64  `json:"newest_archive_size_bytes"`
	VMID                   int      `json:"vmid"`
	Name                   string   `json:"name"`
	Job                    *string  `json:"job"`
	NewestArchiveAgeDays   *int     `json:"newest_archive_age_days"`
	Silenced               bool     `json:"silenced"`
}
type Unit struct {
	Host     string `json:"host"`
	Unit     string `json:"unit"`
	Silenced bool   `json:"silenced"`
}
type Recent struct {
	Status          string       `json:"status"`
	Error           string       `json:"error,omitempty"`
	WindowS         int          `json:"window_s"`
	Count           *int         `json:"count"`
	HasRecent       *bool        `json:"has_recent"`
	UnknownAgeCount int          `json:"unknown_age_count"`
	Items           []RecentItem `json:"items"`
}
type RecentItem struct {
	Host    string    `json:"host"`
	Rule    string    `json:"rule"`
	Subject string    `json:"subject"`
	Since   time.Time `json:"since"`
}
