package collect

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
	"github.com/ficaa1/homelab-inventory/internal/sysexec"
)

type DiskHealthCollector struct{}

func (DiskHealthCollector) Name() string { return "disks" }

// Probe independently of HostCollector so -only and ordering do not matter.
func diskContainer(ctx context.Context, env *Env) bool {
	if s, err := env.ReadFile("/run/systemd/container"); err == nil && strings.TrimSpace(s) != "" {
		return true
	}
	if env.Run.Available("systemd-detect-virt") {
		r, err := env.Run.Run(ctx, "systemd-detect-virt", "--container")
		return err == nil && r.OK() && strings.TrimSpace(r.Stdout) != "none"
	}
	return false
}
func (DiskHealthCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	if runtime.GOOS != "linux" || diskContainer(ctx, env) {
		return nil
	}
	d := &inventory.DiskHealth{Devices: []inventory.Disk{}}
	inv.Disks = d
	if !env.Run.Available("smartctl") {
		env.Infof("smartctl not installed")
		return nil
	}
	r, err := env.Run.Run(ctx, "smartctl", "--scan")
	if err != nil || !r.OK() {
		env.Infof("SMART scan unavailable")
		return nil
	}
	d.Available = true
	seen := map[string]bool{}
	for _, line := range r.Lines() {
		f := strings.Fields(line)
		if len(f) == 0 || !sysexec.SmartDevice(f[0]) || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		disk := inventory.Disk{Name: f[0], ReadStatus: "failed"}
		d.Devices = append(d.Devices, disk)
		slot := &d.Devices[len(d.Devices)-1]
		r, err := env.Run.Run(ctx, "smartctl", "-H", "-A", "-j", "-n", "standby", f[0])
		if err != nil {
			env.Infof("SMART read unavailable for %s", f[0])
			continue
		}
		// smartctl's health-failure bits are data, not a failed collection.
		parsed, ok, err := parseSMART(f[0], r.Stdout)
		if err != nil {
			env.Infof("SMART JSON unavailable for %s", f[0])
			continue
		}
		disk = parsed
		if r.ExitCode == 2 && disk.ReadStatus == "standby" {
			*slot = disk
			continue
		}
		if r.ExitCode&3 != 0 {
			env.Infof("SMART read unavailable for %s", f[0])
			continue
		}
		*slot = disk
		if ok {
			if disk.Model == "" {
				node := filepath.Base(f[0])
				for _, path := range []string{"/sys/class/block/" + node + "/device/model", "/sys/class/nvme/" + node + "/model"} {
					if model, err := env.ReadFile(path); err == nil && strings.TrimSpace(model) != "" {
						disk.Model = redact.Text(strings.TrimSpace(model))
						break
					}
				}
			}
			*slot = disk
		}
	}
	return nil
}
func parseSMART(name, raw string) (inventory.Disk, bool, error) {
	var s struct {
		Smartctl struct {
			Messages []struct {
				String string `json:"string"`
			} `json:"messages"`
		} `json:"smartctl"`
		Model  string `json:"model_name"`
		Health *struct {
			Passed *bool `json:"passed"`
		} `json:"smart_status"`
		ATA *struct {
			Table []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
				Raw  struct {
					Value  *uint64 `json:"value"`
					String string  `json:"string"`
				} `json:"raw"`
			} `json:"table"`
		} `json:"ata_smart_attributes"`
		NVMe *struct {
			MediaErrors *uint64 `json:"media_errors"`
			Used        *uint64 `json:"percentage_used"`
			Hours       *uint64 `json:"power_on_hours"`
			Temp        *int    `json:"temperature"`
		} `json:"nvme_smart_health_information_log"`
		Temp struct {
			Current *int `json:"current"`
		} `json:"temperature"`
		Power struct {
			Hours *uint64 `json:"hours"`
		} `json:"power_on_time"`
	}
	d := inventory.Disk{Name: name, ReadStatus: "unsupported"}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return d, false, err
	}
	for _, m := range s.Smartctl.Messages {
		if strings.Contains(strings.ToLower(m.String), "standby") {
			d.ReadStatus = "standby"
			return d, false, nil
		}
	}
	if s.Health == nil && s.ATA == nil && s.NVMe == nil {
		return d, false, nil
	}
	d.ReadStatus = "ok"
	d.Model = redact.Text(s.Model)
	if s.Health != nil {
		d.Passed = s.Health.Passed
	}
	d.Temperature = s.Temp.Current
	d.PowerOnHours = s.Power.Hours
	if s.ATA != nil {
		for _, a := range s.ATA.Table {
			n := smartRawCounter(a.Raw.Value, a.Raw.String)
			switch {
			case a.ID == 5 && a.Name == "Reallocated_Sector_Ct":
				d.ReallocatedSectors = n
			case a.ID == 197 && a.Name == "Current_Pending_Sector":
				d.PendingSectors = n
			case a.ID == 198 && a.Name == "Offline_Uncorrectable":
				d.OfflineUncorrectable = n
			}
		}
	}
	if s.NVMe != nil {
		d.MediaErrors = s.NVMe.MediaErrors
		d.PercentageUsed = s.NVMe.Used
		d.PowerOnHours = s.NVMe.Hours
		d.Temperature = s.NVMe.Temp
	}
	return d, true, nil
}

// A leading decimal counter in smartctl's display excludes vendor-packed words.
// Unfamiliar displays stay unknown rather than interpreting packed raw.value.
func smartRawCounter(value *uint64, display string) *uint64 {
	if display == "" {
		return value
	}
	fields := strings.Fields(display)
	if len(fields) == 0 {
		return nil
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return nil
	}
	return &n
}
