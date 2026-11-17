package cve

import (
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"strconv"
	"strings"
	"time"
)

func ecosystem(d plugin.PackageDistro) (string, string) {
	v := d.VersionID
	major := strings.Split(v, ".")[0]
	valid := func(s string) bool { n, err := strconv.Atoi(s); return err == nil && n > 0 }
	switch strings.ToLower(d.ID) {
	case "debian":
		if valid(major) {
			return "Debian:" + major, ""
		}
	case "ubuntu":
		p := strings.Split(v, ".")
		if len(p) == 2 && len(p[0]) == 2 && len(p[1]) == 2 && valid(p[0]) && valid(p[1]) {
			yy, _ := strconv.Atoi(p[0])
			mm, _ := strconv.Atoi(p[1])
			if mm > 12 {
				return "", "invalid Ubuntu version"
			}
			if p[1] == "04" && yy%2 == 0 {
				return "Ubuntu:" + v + ":LTS", ""
			}
			return "Ubuntu:" + v, ""
		}
	case "alpine":
		p := strings.Split(v, ".")
		if len(p) >= 2 && valid(p[0]) && valid(p[1]) {
			return "Alpine:v" + p[0] + "." + p[1], ""
		}
	case "rocky":
		if valid(major) {
			return "Rocky Linux:" + major, ""
		}
	case "almalinux":
		if valid(major) {
			return "AlmaLinux:" + major, ""
		}
	}
	return "", "unsupported distro or version"
}

// Debian dates include LTS; Ubuntu dates are standard support only. Future
// releases carry scheduled dates and should be refreshed as vendors revise them.
var eolDates = map[string]string{
	"Debian:10": "2024-06-30", "Debian:11": "2026-08-31", "Debian:12": "2028-06-30", "Debian:13": "2030-06-30",
	"Ubuntu:20.04:LTS": "2025-05-31", "Ubuntu:20.10": "2021-07-22", "Ubuntu:21.04": "2022-01-20", "Ubuntu:21.10": "2022-07-14",
	"Ubuntu:22.04:LTS": "2027-05-31", "Ubuntu:22.10": "2023-07-20", "Ubuntu:23.04": "2024-01-25", "Ubuntu:23.10": "2024-07-11",
	"Ubuntu:24.04:LTS": "2029-05-31", "Ubuntu:24.10": "2025-07-10", "Ubuntu:25.04": "2026-01-15", "Ubuntu:25.10": "2026-07-09", "Ubuntu:26.04:LTS": "2031-05-31",
	"Alpine:v3.17": "2024-11-22", "Alpine:v3.18": "2025-05-09", "Alpine:v3.19": "2025-11-01", "Alpine:v3.20": "2026-04-01",
	"Alpine:v3.21": "2026-11-01", "Alpine:v3.22": "2027-05-01", "Alpine:v3.23": "2027-11-01", "Alpine:v3.24": "2028-06-01",
}

func distroEOL(eco string, now time.Time) *EOL {
	date := eolDates[eco]
	if date != "" && now.UTC().Format("2006-01-02") > date {
		return &EOL{Date: date}
	}
	return nil
}
func distroLabel(d plugin.PackageDistro) string {
	name := map[string]string{"debian": "Debian", "ubuntu": "Ubuntu", "alpine": "Alpine", "rocky": "Rocky Linux", "almalinux": "AlmaLinux"}[d.ID]
	if name == "" {
		name = d.ID
	}
	s := fmt.Sprintf("%s %s", name, d.VersionID)
	if d.Codename != "" {
		s += " (" + d.Codename + ")"
	}
	return s
}
