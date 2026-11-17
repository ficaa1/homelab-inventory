package cve

import (
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"time"
)

type Counts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
}

func (c *Counts) add(s string) {
	switch s {
	case "critical":
		c.Critical++
	case "high":
		c.High++
	case "medium":
		c.Medium++
	case "low":
		c.Low++
	default:
		c.Unknown++
	}
}

type Suppression struct {
	State  string    `json:"state"`
	Reason string    `json:"reason"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Until  *string   `json:"until"`
}
type CVSS struct {
	Score   float64 `json:"score"`
	Vector  string  `json:"vector"`
	Version string  `json:"version"`
}
type EPSS struct {
	Score      float64 `json:"score"`
	Percentile float64 `json:"percentile"`
}
type KEV struct {
	DateAdded  string `json:"date_added"`
	DueDate    string `json:"due_date"`
	Ransomware bool   `json:"ransomware"`
	Name       string `json:"name"`
}
type VulnRow struct {
	ID             string       `json:"id"`
	OSVIDs         []string     `json:"osv_ids"`
	Summary        string       `json:"summary"`
	Severity       string       `json:"severity"`
	SeveritySource string       `json:"severity_source"`
	CVSS           *CVSS        `json:"cvss"`
	EPSS           *EPSS        `json:"epss"`
	KEV            *KEV         `json:"kev"`
	Published      *time.Time   `json:"published"`
	Fix            string       `json:"fix"`
	Hosts          int          `json:"hosts"`
	Sets           int          `json:"sets"`
	Packages       []string     `json:"packages"`
	Priority       int          `json:"priority"`
	Suppressed     *Suppression `json:"suppressed"`
}
type Reference struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}
type Binary struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type Affected struct {
	Host         string   `json:"host"`
	Scope        string   `json:"scope"`
	ImageRef     string   `json:"image_ref"`
	Containers   []string `json:"containers"`
	Ecosystem    string   `json:"ecosystem"`
	Package      string   `json:"package"`
	Binaries     []Binary `json:"binaries"`
	Installed    string   `json:"installed"`
	Fixed        *string  `json:"fixed"`
	Fixable      bool     `json:"fixable"`
	VendorStatus *string  `json:"vendor_status"`
}
type Vulnerability struct {
	VulnRow
	// Details and References are filled from the advisory cache only for the
	// single-CVE endpoint; the stored result omits them to stay small.
	Details    string      `json:"details"`
	Aliases    []string    `json:"aliases"`
	CWEIDs     []string    `json:"cwe_ids"`
	References []Reference `json:"references"`
	Affected   []Affected  `json:"affected"`
	// The highest distro rating seen while grouping; it outranks CVSS.
	vendor, vendorSource string
}
type SetRef struct {
	Host     string `json:"host"`
	Scope    string `json:"scope"`
	ImageRef string `json:"image_ref"`
}
type PackageRow struct {
	Ecosystem  string   `json:"ecosystem"`
	Name       string   `json:"name"`
	Installed  []string `json:"installed"`
	Hosts      int      `json:"hosts"`
	Sets       int      `json:"sets"`
	SetRefs    []SetRef `json:"set_refs"`
	Vulns      Counts   `json:"vulns"`
	KEV        int      `json:"kev"`
	Fixable    int      `json:"fixable"`
	FixVersion *string  `json:"fix_version"`
	Fixes      int      `json:"fixes"`
	Top        []string `json:"top"`
	Priority   int      `json:"priority"`
}
type EOL struct {
	Date string `json:"date"`
}
type HostRow struct {
	Host        string               `json:"host"`
	Scope       string               `json:"scope"`
	ImageRef    string               `json:"image_ref"`
	Containers  []string             `json:"containers"`
	Ecosystem   *string              `json:"ecosystem"`
	Distro      plugin.PackageDistro `json:"distro"`
	EOL         *EOL                 `json:"eol"`
	CollectedAt time.Time            `json:"collected_at"`
	Packages    int                  `json:"packages"`
	Status      string               `json:"status"`
	Reason      *string              `json:"reason"`
	Vulns       Counts               `json:"vulns"`
	KEV         int                  `json:"kev"`
	Fixable     int                  `json:"fixable"`
}
type Unsupported struct {
	Host     string `json:"host"`
	Scope    string `json:"scope"`
	ImageRef string `json:"image_ref"`
	Reason   string `json:"reason"`
}
type Coverage struct {
	Hosts       int           `json:"hosts"`
	Sets        int           `json:"sets"`
	SetsScanned int           `json:"sets_scanned"`
	Packages    int           `json:"packages"`
	Unsupported []Unsupported `json:"unsupported"`
	// Skipped lists source packages OSV could not answer for within the
	// client timeout (the Ubuntu kernel's first page takes over ten seconds).
	Skipped []Skipped `json:"skipped"`
}
type Skipped struct {
	Ecosystem string `json:"ecosystem"`
	Package   string `json:"package"`
	Version   string `json:"version"`
	Sets      int    `json:"sets"`
	Reason    string `json:"reason"`
}
type SourceStatus struct {
	OK        bool       `json:"ok"`
	UpdatedAt *time.Time `json:"updated_at"`
	Error     *string    `json:"error"`
}
type KEVStatus struct {
	SourceStatus
	Count   int  `json:"count"`
	Enabled bool `json:"enabled"`
}
type EPSSStatus struct {
	SourceStatus
	Enabled bool `json:"enabled"`
}
type Sources struct {
	OSV  SourceStatus `json:"osv"`
	KEV  KEVStatus    `json:"kev"`
	EPSS EPSSStatus   `json:"epss"`
}
type Progress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}
type LastScan struct {
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	DurationMS int64      `json:"duration_ms"`
	Error      *string    `json:"error"`
}
type Status struct {
	State      string     `json:"state"`
	Phase      *string    `json:"phase"`
	Progress   *Progress  `json:"progress"`
	LastScan   *LastScan  `json:"last_scan"`
	NextScanAt *time.Time `json:"next_scan_at"`
	Sources    Sources    `json:"sources"`
	Coverage   Coverage   `json:"coverage"`
}
type Totals struct {
	Vulns            int    `json:"vulns"`
	KEV              int    `json:"kev"`
	Fixable          int    `json:"fixable"`
	HostsAffected    int    `json:"hosts_affected"`
	SetsAffected     int    `json:"sets_affected"`
	PackagesAffected int    `json:"packages_affected"`
	Suppressed       int    `json:"suppressed"`
	Severity         Counts `json:"severity"`
}
type EOLRow struct {
	SetRef
	Distro  string `json:"distro"`
	EOLDate string `json:"eol_date"`
}
type Overview struct {
	GeneratedAt *time.Time   `json:"generated_at"`
	Totals      Totals       `json:"totals"`
	TopFixes    []PackageRow `json:"top_fixes"`
	EOL         []EOLRow     `json:"eol"`
}

// Result is published atomically after a complete match/detail pass. API reads
// never mutate it; suppressions and cached enrichment are layered on snapshots.
type Result struct {
	GeneratedAt *time.Time      `json:"generated_at"`
	Vulns       []Vulnerability `json:"vulns"`
	Hosts       []HostRow       `json:"hosts"`
	Coverage    Coverage        `json:"coverage"`
}
type enrichment struct {
	KEV         map[string]KEV       `json:"kev"`
	EPSS        map[string]EPSS      `json:"epss"`
	EPSSTimes   map[string]time.Time `json:"epss_times"`
	KEVAttempt  *time.Time           `json:"kev_attempt"`
	EPSSAttempt *time.Time           `json:"epss_attempt"`
	Sources     Sources              `json:"sources"`
}
