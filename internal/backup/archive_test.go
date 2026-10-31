package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func put(t *testing.T, root, name, value string) {
	t.Helper()
	p := filepath.Join(root, name)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(value), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestRoundTripAndRollback(t *testing.T) {
	src := t.TempDir()
	files := map[string]string{"host/20260101T000000.000000000Z.json": "{\"future\":true}", "host/software/history.jsonl": "{}\n{}\n", "packagesets/abc.json.gz": "opaque compressed bytes", "state-notify.json": "{\"seeded\":true}", "plugins/dusk/core/policy.json": "{\"receipts\":{\"pending\":true}}"}
	for p, v := range files {
		put(t, src, p, v)
	}
	var b bytes.Buffer
	_, e := Create(context.Background(), src, "test", map[string]string{"secret": "synthetic"}, &b, DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	p, e := Stage(context.Background(), &b, t.TempDir(), DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	dest := t.TempDir()
	put(t, dest, "old.json", "old")
	bad := errors.New("reopen failed")
	if e = Install(dest, p.Directory, func() error { return bad }); !errors.Is(e, bad) {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(filepath.Join(dest, "old.json")); e != nil || string(b) != "old" {
		t.Fatal("rollback", e)
	}
	if _, e = os.Stat(filepath.Join(dest, "host")); !os.IsNotExist(e) {
		t.Fatal("new generation survived rollback")
	}
}
func TestEnvelope(t *testing.T) {
	var b bytes.Buffer
	pass := "synthetic long passphrase"
	if e := Encrypt(context.Background(), &b, pass, func(w io.Writer) error { _, e := w.Write([]byte("secret data")); return e }); e != nil {
		t.Fatal(e)
	}
	original := append([]byte(nil), b.Bytes()...)
	for _, tc := range []struct {
		name, pass string
		mutate     func([]byte) []byte
	}{{"good", pass, func(b []byte) []byte { return b }}, {"wrong", "wrong passphrase", func(b []byte) []byte { return b }}, {"tamper", pass, func(b []byte) []byte { b[headerSize] ^= 1; return b }}, {"truncated", pass, func(b []byte) []byte { return b[:len(b)-1] }}} {
		t.Run(tc.name, func(t *testing.T) {
			r, clean, e := Decrypt(context.Background(), bytes.NewReader(tc.mutate(append([]byte(nil), original...))), t.TempDir(), tc.pass, 10000)
			if tc.name != "good" {
				if e == nil {
					clean()
					t.Fatal("accepted damaged envelope")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			defer clean()
			got, e := io.ReadAll(r)
			if e != nil || string(got) != "secret data" {
				t.Fatal(e, string(got))
			}
		})
	}
}
func TestRejectUnsafeArchives(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a\\b", ".backup-journal.json", "host/../../escape"} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			tw := tar.NewWriter(gz)
			m, _ := json.Marshal(Manifest{Version: Version, Files: []Entry{{Path: name, Size: 0, SHA256: string(bytes.Repeat([]byte("a"), 64))}}})
			tw.WriteHeader(&tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Size: int64(len(m))})
			tw.Write(m)
			tw.Close()
			gz.Close()
			if _, e := Stage(context.Background(), &b, t.TempDir(), DefaultLimits()); e == nil {
				t.Fatal("accepted unsafe path")
			}
		})
	}
}
func TestCanceledAndLimits(t *testing.T) {
	root := t.TempDir()
	put(t, root, "state.json", "data")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := Create(ctx, root, "test", nil, io.Discard, DefaultLimits()); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e := Create(context.Background(), root, "test", nil, io.Discard, Limits{1, 2, 2}); e == nil {
		t.Fatal("limit ignored")
	}
}
func TestRecoveryBoundaries(t *testing.T) {
	for _, step := range []string{"journal", "moved-old", "installed"} {
		t.Run(step, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "old", "old")
			rb, e := os.MkdirTemp(root, ".backup-rollback-")
			if e != nil {
				t.Fatal(e)
			}
			j := journal{Rollback: filepath.Base(rb), Old: []string{"old"}, New: []string{"new"}}
			if e = writeJournal(root, j); e != nil {
				t.Fatal(e)
			}
			if step != "journal" {
				if e = os.Rename(filepath.Join(root, "old"), filepath.Join(rb, "old")); e != nil {
					t.Fatal(e)
				}
			}
			if step == "installed" {
				put(t, root, "new", "new")
			}
			if e = Recover(root); e != nil {
				t.Fatal(e)
			}
			got, e := os.ReadFile(filepath.Join(root, "old"))
			if e != nil || string(got) != "old" {
				t.Fatal("old generation lost", e)
			}
			if _, e = os.Stat(filepath.Join(root, "new")); !os.IsNotExist(e) {
				t.Fatal("new survived")
			}
		})
	}
}
func TestHostileArchiveEntries(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		kind                    byte
		duplicate, bomb, future bool
	}{{"symlink", tar.TypeSymlink, false, false, false}, {"hardlink", tar.TypeLink, false, false, false}, {"device", tar.TypeChar, false, false, false}, {"duplicate", tar.TypeReg, true, false, false}, {"trailing-bomb", tar.TypeReg, false, true, false}, {"future", tar.TypeReg, false, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			tw := tar.NewWriter(gz)
			h := sha256.Sum256(nil)
			m := Manifest{Version: Version, Files: []Entry{{Path: "state.json", Size: 0, SHA256: hex.EncodeToString(h[:])}}}
			if tc.future {
				m.Version++
			}
			raw, _ := json.Marshal(m)
			tw.WriteHeader(&tar.Header{Name: ManifestName, Typeflag: tar.TypeReg, Size: int64(len(raw))})
			tw.Write(raw)
			tw.WriteHeader(&tar.Header{Name: "state.json", Typeflag: tc.kind, Linkname: "/etc/passwd"})
			if tc.duplicate {
				tw.WriteHeader(&tar.Header{Name: "state.json", Typeflag: tar.TypeReg})
			}
			tw.Close()
			if tc.bomb {
				gz.Write(make([]byte, 1<<20))
			}
			gz.Close()
			if _, e := Stage(context.Background(), &b, t.TempDir(), DefaultLimits()); e == nil {
				t.Fatal("hostile archive accepted")
			}
		})
	}
}
func TestSealDetectsPreparedTampering(t *testing.T) {
	p := Plan{Directory: t.TempDir(), Manifest: Manifest{Version: Version}}
	put(t, p.Directory, "state.json", "old")
	if e := Seal(context.Background(), &p); e != nil {
		t.Fatal(e)
	}
	put(t, p.Directory, "state.json", "changed")
	if e := Verify(context.Background(), p); e == nil {
		t.Fatal("tampered plan accepted")
	}
}
