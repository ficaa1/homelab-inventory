//go:build agentinstalltest

package agentmount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Synthetic mounts are available only in the disposable integration executable.
func init() {
	testDatasetCheck = func(dir string) (bool, error) {
		path := os.Getenv("AGENTINSTALL_TEST_MOUNTINFO")
		if path == "" {
			return false, nil
		}
		root := filepath.Dir(path)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(root) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(root), "inventory-install-") {
			return true, errors.New("test dataset fixture must be disposable")
		}
		if err := checkDatasetPath(dir, root); err != nil {
			return true, err
		}
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
			return true, errors.New("invalid synthetic mount fixture")
		}
		f, e := os.Open(path)
		if e != nil {
			return true, errors.New("cannot open synthetic mount fixture")
		}
		defer f.Close()
		return true, checkMountBelow(dir, f, root)
	}
}
