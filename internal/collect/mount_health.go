package collect

import (
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"strings"
)

func mountOption(options, want string) bool {
	for _, o := range strings.Split(options, ",") {
		if o == want {
			return true
		}
	}
	return false
}

func annotateMounts(env *Env, mounts []inventory.Filesystem) {
	fstab, _ := env.ReadFile("/etc/fstab")
	info, _ := env.ReadFile("/proc/self/mountinfo")
	for i := range mounts {
		m := &mounts[i]
		for _, line := range strings.Split(fstab, "\n") {
			f := strings.Fields(line)
			if len(f) >= 4 && !strings.HasPrefix(f[0], "#") && unescapeMount(f[1]) == m.MountPoint {
				m.ConfiguredReadOnly = mountOption(f[3], "ro")
				m.BindMount = mountOption(f[3], "bind") || mountOption(f[3], "rbind")
			}
		}
		// mountinfo's root identifies subdirectory/file bind mounts even though
		// /proc/mounts does not expose the bind flag. '/' LXC roots remain eligible.
		for _, line := range strings.Split(info, "\n") {
			f := strings.Fields(line)
			if len(f) >= 6 && unescapeMount(f[4]) == m.MountPoint && f[3] != "/" && m.FSType != "btrfs" {
				m.BindMount = true
			}
		}
	}
}
