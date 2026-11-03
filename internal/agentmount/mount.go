// Package agentmount validates persistent executable agent state directories.
package agentmount

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Set only by the explicitly tagged integration executable.
var testDatasetCheck func(string) (bool, error)

// CheckDataset checks the actual mount covering dir before the caller creates it.
// A missing leaf is allowed, but every existing component must be a directory and
// no component may be a symlink. The covering mount must be a writable executable
// dataset below /mnt, rather than the root filesystem or an unmounted pool path.
// Call this again at runtime, before accessing state, to detect an unmounted pool.
func CheckDataset(dir string) error {
	if testDatasetCheck != nil {
		if handled, e := testDatasetCheck(dir); handled {
			return e
		}
	}
	if err := checkDatasetPath(dir, "/mnt"); err != nil {
		return err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return errors.New("cannot read agent dataset mounts")
	}
	defer f.Close()
	return checkMount(dir, f)
}

func checkDatasetPath(dir, parent string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || !beneath(dir, parent) || dir == parent || strings.ContainsAny(dir, "\x00\r\n%") {
		return errors.New("agent dataset must be an absolute clean path below /mnt")
	}
	path := string(filepath.Separator)
	missing := false
	for _, part := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		path = filepath.Join(path, part)
		if missing {
			continue
		}
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			missing = true
			continue
		}
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errors.New("agent dataset contains an unsafe path component")
		}
	}
	return nil
}

// The parser is separate so synthetic mount fixtures can exercise it without
// overriding /proc paths or adding a production mount-validation bypass.
func checkMount(dir string, mounts io.Reader) error { return checkMountBelow(dir, mounts, "/mnt") }
func checkMountBelow(dir string, mounts io.Reader, parent string) error {
	s := bufio.NewScanner(mounts)
	s.Buffer(make([]byte, 4096), 1<<20)
	var rootDevice, device, mount, options, superOptions, filesystem string
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 10 {
			return errors.New("invalid agent dataset mount information")
		}
		sep := 6
		for sep < len(fields) && fields[sep] != "-" {
			sep++
		}
		if sep+3 >= len(fields) {
			return errors.New("invalid agent dataset mount information")
		}
		point, ok := mountPath(fields[4])
		if !ok || !filepath.IsAbs(point) || filepath.Clean(point) != point {
			return errors.New("invalid agent dataset mount information")
		}
		if point == "/" {
			rootDevice = fields[2]
		}
		// Last entry wins for stacked mounts at the same path.
		if beneath(dir, point) && len(point) >= len(mount) {
			mount, device, options, superOptions, filesystem = point, fields[2], fields[5], fields[sep+3], fields[sep+1]
		}
	}
	if s.Err() != nil {
		return errors.New("cannot read agent dataset mounts")
	}
	if dir == mount {
		return errors.New("state directory must be a dedicated subdirectory, not a pool or dataset mountpoint")
	}
	if filesystem != "zfs" || rootDevice == "" || device == rootDevice || mount == parent || !beneath(mount, parent) {
		return errors.New("agent dataset is not on a mounted non-root dataset below /mnt")
	}
	for _, opts := range []string{options, superOptions} {
		for _, opt := range strings.Split(opts, ",") {
			if opt == "noexec" || opt == "ro" {
				return errors.New("agent dataset mount must be writable and executable")
			}
		}
	}
	return checkZFSMountpoints(dir)
}

// A mounted parent does not prove that a locked/noauto child is mounted. Files
// written into its empty mountpoint would be hidden when it later mounts.
func checkZFSMountpoints(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "zfs", "list", "-H", "-o", "name,mountpoint,mounted")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("cannot inspect configured ZFS dataset mountpoints")
	}
	if err = cmd.Start(); err != nil {
		return errors.New("cannot inspect configured ZFS dataset mountpoints")
	}
	// Always reap the bounded read-only command, including on parse failures.
	defer func() { cancel(); _ = cmd.Wait() }()
	limited := &io.LimitedReader{R: pipe, N: 4 << 20}
	scanner := bufio.NewScanner(limited)
	rows := 0
	for scanner.Scan() {
		rows++
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 3 {
			return errors.New("invalid ZFS dataset mountpoint information")
		}
		point := fields[1]
		if filepath.IsAbs(point) && beneath(dir, filepath.Clean(point)) && fields[2] != "yes" {
			return errors.New("state directory is at or below an unmounted ZFS dataset; mount it first or choose a new path")
		}
	}
	if scanner.Err() != nil || limited.N == 0 || rows == 0 {
		return errors.New("cannot read ZFS dataset mountpoints")
	}
	if err := cmd.Wait(); err != nil {
		return errors.New("cannot inspect configured ZFS dataset mountpoints")
	}
	return nil
}

func beneath(path, parent string) bool {
	return path == parent || parent == "/" && strings.HasPrefix(path, "/") || strings.HasPrefix(path, parent+"/")
}

func mountPath(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+3 >= len(s) {
			return "", false
		}
		switch s[i+1 : i+4] {
		case "040":
			b.WriteByte(' ')
		case "011":
			b.WriteByte('\t')
		case "012":
			b.WriteByte('\n')
		case "134":
			b.WriteByte('\\')
		default:
			return "", false
		}
		i += 3
	}
	return b.String(), true
}

// Marker identifies a dedicated installation directory on subsequent pastes.
const Marker = ".homelab-inventory"

// ValidateStatePath is also used by the API, which cannot inspect a remote host.
func ValidateStatePath(dir string) error {
	plain := true
	for _, c := range dir {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/_.-", c)) {
			plain = false
			break
		}
	}
	if !plain || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || !strings.HasPrefix(dir, "/mnt/") || len(strings.Split(strings.Trim(dir, "/"), "/")) < 3 {
		return errors.New("TrueNAS state_dir must be a dedicated plain path below /mnt/POOL, never a pool or dataset root; choose a new path or an existing root-owned 0700 empty directory")
	}
	return nil
}

// CheckDedicated must run before mkdir, chown or chmod on TrueNAS state.
func CheckDedicated(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect dedicated state directory")
	}
	if len(entries) == 0 {
		st, err := os.Lstat(dir)
		if err == nil && st.IsDir() && st.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0700 {
			if owner, ok := st.Sys().(*syscall.Stat_t); ok && owner.Uid == 0 {
				return nil
			}
		}
		return errors.New("existing empty state directory must already be root-owned with mode 0700; give a new path")
	}
	st, err := os.Lstat(filepath.Join(dir, Marker))
	if err == nil && st.Mode().IsRegular() {
		b, err := os.ReadFile(filepath.Join(dir, Marker))
		if err == nil && string(b) == "homelab-inventory\n" {
			return nil
		}
	}
	return errors.New("state directory must be new, root-owned 0700 and empty, or marked by homelab-inventory; give a new path")
}

// IsTrueNAS avoids invoking middleware just to identify the host.
func IsTrueNAS() bool {
	if _, err := exec.LookPath("midclt"); err != nil {
		return false
	}
	if b, err := os.ReadFile("/etc/version"); err == nil && strings.Contains(strings.ToLower(string(b)), "truenas") {
		return true
	}
	_, err := os.Stat("/usr/bin/middlewared")
	return err == nil
}
