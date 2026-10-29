package collect

import (
	"context"
	"encoding/json"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
	"github.com/ficaa1/homelab-inventory/internal/sysexec"
)

type KernelIOCollector struct{}

func (KernelIOCollector) Name() string { return "kernel_io" }
func (KernelIOCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	if runtime.GOOS != "linux" || diskContainer(ctx, env) {
		return nil
	}
	k := &inventory.KernelIO{Devices: []inventory.KernelIODevice{}}
	inv.KernelIO = k
	if boot, err := env.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if id := strings.TrimSpace(boot); kernelBootID.MatchString(id) {
			k.BootID = id
		}
	}
	if !env.Run.Available("journalctl") {
		env.Infof("kernel journal unavailable")
		return nil
	}
	r, err := env.Run.Run(ctx, "journalctl", sysexec.KernelJournalArgs()...)
	if err != nil || !r.OK() || strings.TrimSpace(r.Stderr) != "" {
		env.Infof("kernel journal unavailable")
		return nil
	}
	observed := inv.CollectedAt
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	devices, valid := parseKernelIOAt(r.Stdout, observed)
	if !valid {
		env.Infof("kernel journal incomplete")
		return nil
	}
	k.Available = true
	k.ObservedAt = observed
	k.Devices = devices
	return nil
}

var kernelBootID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var ioError = regexp.MustCompile(`(?i)(\bblk_update_request:.*(I/O error|critical medium error)|\bcritical medium error\b|\bI/O error, dev [a-z0-9-]+, sector\b|\bBuffer I/O error on dev\b|\bEXT4-fs error\b|\bEXT4-fs \([^)]+\): Remounting filesystem read-only|\bXFS \([^)]+\): metadata I/O error|\b(?:scsi|sd) [0-9]+:[0-9]+:[0-9]+:[0-9]+:.*(?:FAILED Result:|\babort\b)|\bata[0-9.]+:.*failed command)`)
// Optical, floppy and loop devices are not storage health (Proxmox VMs log fd0 errors at boot).
var opticalDevice = regexp.MustCompile(`\b(?:sr[0-9]+|cdrom[0-9]*|fd[0-9]+|loop[0-9]+)\b`)
var ioDevice = regexp.MustCompile(`(?:\bdev(?:ice)?[ :]+|\bEXT4-fs (?:(?:error|warning) \(device |\()|\bXFS \(|\[)((?:sd|hd|vd)[a-z]+[0-9]*|nvme[0-9]+n[0-9]+(?:p[0-9]+)?|dm-[0-9]+|md[0-9]+)\b`)
var ioBus = regexp.MustCompile(`\b(ata[0-9.]+|(?:scsi|sd) [0-9]+:[0-9]+:[0-9]+:[0-9]+)\b`)

// Journal JSON avoids parsing locale-dependent timestamp prefixes. Unknown
// device lines are retained under 'unknown'; ATA/SCSI transport IDs are stable
// subjects when the kernel gives no block device name.
func parseKernelIO(raw string) ([]inventory.KernelIODevice, bool) {
	return parseKernelIOAt(raw, time.Time{})
}
func parseKernelIOAt(raw string, observed time.Time) ([]inventory.KernelIODevice, bool) {
	out := []inventory.KernelIODevice{}
	by := map[string]*inventory.KernelIODevice{}
	newest := map[string]uint64{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			Message string `json:"MESSAGE"`
			At      string `json:"__REALTIME_TIMESTAMP"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			return out, false
		}
		if !ioError.MatchString(entry.Message) || opticalDevice.MatchString(entry.Message) {
			continue
		}
		device := "unknown"
		if m := ioDevice.FindStringSubmatch(entry.Message); m != nil {
			device = m[1]
		} else if m := ioBus.FindString(entry.Message); m != "" {
			device = m
		}
		d := by[device]
		if d == nil {
			d = &inventory.KernelIODevice{Device: device}
			by[device] = d
		}
		at, err := strconv.ParseUint(entry.At, 10, 64)
		if err != nil || at == 0 {
			return out, false
		}
		errorAt := time.UnixMicro(int64(at)).UTC()
		if observed.IsZero() || !errorAt.Before(observed.Add(-20*time.Minute)) {
			d.Count++
		}
		if at >= newest[device] {
			sample := redact.Text(entry.Message)
			if len(sample) > 512 {
				sample = sample[:512]
			}
			d.NewestSample = sample
			d.LastErrorAt = errorAt
			newest[device] = at
		}
	}
	for _, d := range by {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out, true
}
