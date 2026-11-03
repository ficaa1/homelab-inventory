// Package agentmount validates persistent executable agent state directories.
package agentmount

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
