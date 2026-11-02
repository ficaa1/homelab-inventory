package mediapolicy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

func seedUnder(p, root string) bool {
	return root != "/" && filepath.IsAbs(root) && filepath.Clean(root) == root && filepath.IsAbs(p) && filepath.Clean(p) == p && (p == root || strings.HasPrefix(p, root+"/"))
}
func seedRelative(p string) bool {
	return p != "." && p != "" && !strings.ContainsAny(p, "\\\x00") && filepath.IsLocal(p) && filepath.Clean(p) == p
}
func seedMap(p string, mounts []archivetier.MountMapping) (string, error) {
	found := ""
	for _, m := range mounts {
		if !seedUnder(m.LocalRoot, m.LocalRoot) || !seedUnder(m.AppRoot, m.AppRoot) {
			return "", fmt.Errorf("invalid seeding mapping")
		}
		if seedUnder(p, m.AppRoot) {
			if found != "" {
				return "", fmt.Errorf("ambiguous seeding mapping")
			}
			rel, _ := filepath.Rel(m.AppRoot, p)
			found = filepath.Join(m.LocalRoot, rel)
		}
	}
	if found == "" {
		return "", fmt.Errorf("seeding inspection mount missing")
	}
	return found, nil
}
func seedEvidence(p string, size int64) (archivetier.Evidence, error) {
	e, err := (archivetier.LocalFilesystem{}).Stat(p)
	if err != nil || e.Directory || e.Size != size {
		return e, fmt.Errorf("seeding file missing, changed or symlinked")
	}
	return e, nil
}

// qBittorrent performs move-mode filesystem writes outside our descriptor API.
// Check the nearest existing parent and every ancestor before handing it a path.
func seedAbsentDestination(p string) error {
	if !seedUnder(p, p) {
		return fmt.Errorf("unsafe seeding destination")
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		return fmt.Errorf("seeding destination already exists or is unavailable")
	}
	for parent := filepath.Dir(p); parent != "/"; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(parent); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("seeding destination parent unavailable")
		}
		ev, err := (archivetier.LocalFilesystem{}).Stat(parent)
		if err != nil || !ev.Directory {
			return fmt.Errorf("seeding destination parent is unsafe or symlinked")
		}
		return nil
	}
	return fmt.Errorf("seeding destination has no approved parent")
}

// seedPeerPaths lists where another torrent's file can live: its save path
// and, while it is incomplete, its download path.
func seedPeerPaths(t SeedTorrent, name string) ([]string, error) {
	if !seedRelative(name) || !seedUnder(t.SavePath, t.SavePath) {
		return nil, fmt.Errorf("other torrent has an unsafe path")
	}
	ps := []string{filepath.Join(t.SavePath, name)}
	if t.DownloadPath != nil && *t.DownloadPath != "" && *t.DownloadPath != t.SavePath {
		if !seedUnder(*t.DownloadPath, *t.DownloadPath) {
			return nil, fmt.Errorf("other torrent has an unsafe path")
		}
		ps = append(ps, filepath.Join(*t.DownloadPath, name))
	}
	return ps, nil
}

// seedPeerEvidence inspects another torrent's file. A file that does not
// exist (a missing-files or partly downloaded torrent) holds no inode and so
// shares nothing; any other inspection failure still refuses.
func seedPeerEvidence(p string, mounts []archivetier.MountMapping) (archivetier.Evidence, bool, error) {
	local, err := seedMap(p, mounts)
	if err != nil {
		return archivetier.Evidence{}, false, fmt.Errorf("other torrent file ownership cannot be inspected")
	}
	if _, err := os.Lstat(local); errors.Is(err, fs.ErrNotExist) {
		return archivetier.Evidence{}, false, nil
	}
	ev, err := (archivetier.LocalFilesystem{}).Stat(local)
	if err != nil {
		return archivetier.Evidence{}, false, fmt.Errorf("other torrent file ownership cannot be inspected")
	}
	return ev, true, nil
}
