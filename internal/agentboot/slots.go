package agentboot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentproc"
	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

func platform() string { return runtime.GOOS + "-" + runtime.GOARCH }
func nonce() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func identity(m agentrelease.Manifest, data []byte) agentproto.Identity {
	var e agentrelease.Signed
	_ = json.Unmarshal(data, &e)
	return agentproto.Identity{Sequence: m.Sequence, Digest: agentrelease.Digest(e.Manifest), Version: m.Version, Revision: m.Revision, Discover: m.Artifacts[platform()+"/discover"].SHA256, Updater: m.Artifacts[platform()+"/agentupdate"].SHA256}
}
func (r Runner) verify(data []byte) (agentrelease.Manifest, error) {
	pub, e := readBounded(r.Public, 4096)
	if e != nil {
		return agentrelease.Manifest{}, classify("verify", e)
	}
	m, e := agentrelease.Verify(data, pub)
	if e != nil || m.LauncherProtocol != agentproto.Version {
		return m, failure("verify", "signature_invalid")
	}
	if _, ok := m.Artifacts[platform()+"/agentupdate"]; !ok {
		return m, failure("verify", "platform_missing")
	}
	return m, nil
}
func space(dir string, need uint64) error {
	if e := faultPoint("space"); e != nil {
		return e
	}
	var st syscall.Statfs_t
	if e := syscall.Statfs(dir, &st); e != nil {
		return e
	}
	if e := faultPoint("space-inodes"); e != nil {
		return e
	}
	if uint64(st.Bavail)*uint64(st.Bsize) < need+(agentproto.MaxReport+2<<20) || st.Files > 0 && st.Ffree < 32 {
		return syscall.ENOSPC
	}
	return nil
}
func (r Runner) checkSlot(s Slot) (agentrelease.Manifest, error) {
	data, e := readBounded(filepath.Join(r.slot(s), "runtime.json"), 65536)
	if e != nil {
		return agentrelease.Manifest{}, e
	}
	m, e := r.verify(data)
	if e != nil {
		return m, e
	}
	if identity(m, data) != s.Identity {
		return m, errCorrupt
	}
	for _, role := range []string{"discover", "agentupdate"} {
		a := m.Artifacts[platform()+"/"+role]
		f, e := os.OpenFile(filepath.Join(r.slot(s), role), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return m, e
		}
		st, e := f.Stat()
		if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 {
			f.Close()
			return m, failure("verify", "permission_denied")
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(f, a.Size+1))
		f.Close()
		if e != nil || n != a.Size || fmt.Sprintf("%x", h.Sum(nil)) != a.SHA256 {
			return m, failure("verify", "hash_mismatch")
		}
	}
	return m, nil
}
func (r Runner) stage(ctx context.Context, data []byte, bank, input string) (Slot, error) {
	m, e := r.verify(data)
	if e != nil {
		return Slot{}, e
	}
	s := Slot{Bank: bank, Identity: identity(m, data)}
	if _, e = r.checkSlot(s); e == nil {
		return s, nil
	}
	base := filepath.Join(r.Dir, "slots", bank)
	if e = os.MkdirAll(base, 0700); e != nil {
		return s, e
	}
	for _, dir := range []string{r.Dir, filepath.Join(r.Dir, "slots"), base} {
		if e = syncDir(dir); e != nil {
			return s, e
		}
	}
	var need uint64
	for _, role := range []string{"discover", "agentupdate"} {
		need += uint64(m.Artifacts[platform()+"/"+role].Size)
	}
	if e = space(base, need); e != nil {
		return s, classify("stage_write", e)
	}
	temp, e := os.MkdirTemp(base, ".partial-")
	if e != nil {
		return s, e
	}
	defer removeSlot(temp)
	for _, role := range []string{"discover", "agentupdate"} {
		a := m.Artifacts[platform()+"/"+role]
		var src io.ReadCloser
		if input != "" {
			src, e = os.Open(filepath.Join(input, a.Name))
		} else {
			var respStatus int
			resp, err := r.request(ctx, "GET", "/agent/artifacts/"+strconv.FormatUint(m.Sequence, 10)+"/"+a.Name, nil)
			e = err
			if e == nil {
				src = resp.Body
				respStatus = resp.StatusCode
				if respStatus != 200 {
					src.Close()
					return s, failure("download", "http_status")
				}
			}
		}
		if e != nil {
			return s, classify("download", e)
		}
		f, err := os.OpenFile(filepath.Join(temp, role), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			src.Close()
			return s, err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, a.Size+1))
		src.Close()
		if err == nil && (n != a.Size || fmt.Sprintf("%x", h.Sum(nil)) != a.SHA256) {
			err = failure("verify", "hash_mismatch")
		}
		if err == nil {
			err = f.Chmod(0555)
		}
		if err == nil {
			err = f.Sync()
		}
		ce := f.Close()
		if err == nil {
			err = ce
		}
		if err != nil {
			return s, classify("download", err)
		}
		if e = faultPoint(role + "-fsync"); e != nil {
			return s, e
		}
	}
	if e = r.write(filepath.Join(temp, "runtime.json"), data, 0400, "envelope"); e != nil {
		return s, e
	}
	for _, role := range []string{"discover", "agentupdate"} {
		args := []string{"-list"}
		if role == "agentupdate" {
			args = []string{"-selftest"}
		}
		pre, cancel := context.WithTimeout(ctx, min(r.deadline(), 30*time.Second))
		e = agentproc.Run(pre, filepath.Join(temp, role), args, nil, io.Discard)
		cancel()
		if e != nil {
			code := "selftest_failed"
			if errors.Is(e, context.DeadlineExceeded) {
				code = "selftest_timeout"
			}
			return s, failure("selftest", code)
		}
	}
	if e = os.Chmod(temp, 0555); e != nil {
		return s, e
	}
	if e = syncDir(temp); e != nil {
		return s, e
	}
	if e = faultPoint("slot-before"); e != nil {
		return s, e
	}
	// Never overwrite a published digest, including a corrupt one.
	if _, e = os.Lstat(r.slot(s)); !os.IsNotExist(e) {
		return s, failure("verify", "hash_mismatch")
	}
	if e = os.Rename(temp, r.slot(s)); e != nil {
		return s, e
	}
	if e = faultPoint("slot-rename"); e != nil {
		return s, e
	}
	if e = syncDir(base); e != nil {
		return s, e
	}
	if e = faultPoint("slot-dirsync"); e != nil {
		return s, e
	}
	return s, nil
}
func removeSlot(path string) error {
	if st, e := os.Lstat(path); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return os.Remove(path)
	}
	_ = os.Chmod(path, 0700)
	return os.RemoveAll(path)
}

// Prune only owned slot/partial names, and only while holding the stable lock.
// Keep every runtime referenced by the selected durable control record.
func (r Runner) prune(c Control) error {
	keep := map[string]bool{r.slot(c.Active): true, r.slot(c.Fallback): true}
	if c.Candidate != nil {
		keep[r.slot(*c.Candidate)] = true
	}
	for _, bank := range []string{"a", "b"} {
		base := filepath.Join(r.Dir, "slots", bank)
		entries, e := os.ReadDir(base)
		if e != nil {
			if os.IsNotExist(e) {
				continue
			}
			return e
		}
		for _, entry := range entries {
			name := entry.Name()
			p := filepath.Join(base, name)
			_, e := hex.DecodeString(name)
			owned := strings.HasPrefix(name, ".partial-") || len(name) == 64 && e == nil
			if owned && !keep[p] {
				if e = removeSlot(p); e != nil {
					return e
				}
			}
		}
		if e = syncDir(base); e != nil {
			return e
		}
	}
	if c.State != "report-pending" {
		if e := os.Remove(filepath.Join(r.Dir, "report.json")); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	// Atomic write debris is private and never a recovery source.
	entries, e := os.ReadDir(r.Dir)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".write-") {
			_ = os.Remove(filepath.Join(r.Dir, entry.Name()))
		}
	}
	return nil
}
func (r Runner) Initialize(ctx context.Context, data []byte, input string) error {
	if e := os.MkdirAll(r.Dir, 0700); e != nil {
		return e
	}
	unlock, e := fsx.LockDir(r.Dir)
	if e != nil {
		return e
	}
	defer unlock()
	for _, name := range []string{"control.0", "control.1", "state.json"} {
		if _, e = os.Lstat(filepath.Join(r.Dir, name)); !os.IsNotExist(e) {
			return errors.New("existing state; conversion requires a separate migration")
		}
	}
	a, e := r.stage(ctx, data, "a", input)
	if e != nil {
		return e
	}
	b, e := r.stage(ctx, data, "b", input)
	if e != nil {
		return e
	}
	c := Control{Floor: a.Sequence, FloorDigest: a.Digest, State: "committed", Active: a, Fallback: b, Update: agentproto.Update{Mode: "signed", Sequence: a.Sequence, Result: "installed"}}
	return r.save(&c)
}
