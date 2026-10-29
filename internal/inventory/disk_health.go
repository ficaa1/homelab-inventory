package inventory

import "time"

// DiskHealth is present when SMART collection ran, including unsupported VMs.
type DiskHealth struct {
	Available bool   `json:"available"`
	Devices   []Disk `json:"devices"`
}

// Disk omits serial numbers and raw command output. Nil metrics are unknown.
type Disk struct {
	ReadStatus           string  `json:"read_status,omitempty"` // ok, standby, failed or unsupported
	Name                 string  `json:"name"`
	Model                string  `json:"model,omitempty"`
	Passed               *bool   `json:"passed,omitempty"`
	ReallocatedSectors   *uint64 `json:"reallocated_sectors,omitempty"`
	PendingSectors       *uint64 `json:"pending_sectors,omitempty"`
	OfflineUncorrectable *uint64 `json:"offline_uncorrectable,omitempty"`
	MediaErrors          *uint64 `json:"media_errors,omitempty"`
	PercentageUsed       *uint64 `json:"percentage_used,omitempty"`
	Temperature          *int    `json:"temperature,omitempty"`
	PowerOnHours         *uint64 `json:"power_on_hours,omitempty"`
}

// KernelIO counts recent errors and retains six hours of same-boot evidence.
type KernelIO struct {
	Available  bool             `json:"available"`
	BootID     string           `json:"boot_id,omitempty"`
	ObservedAt time.Time        `json:"observed_at,omitempty"`
	Devices    []KernelIODevice `json:"devices"`
}
type KernelIODevice struct {
	LastErrorAt  time.Time `json:"last_error_at,omitempty"`
	Device       string    `json:"device"`
	Count        uint64    `json:"count"`
	NewestSample string    `json:"newest_sample"`
}
